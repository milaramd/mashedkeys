// mashedkeys counts how many keys are pressed (never which ones) and
// stores per-minute aggregates in SQLite, served as JSON over HTTP.
package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

func main() {
	dbPath := flag.String("db", "mashedkeys.db", "SQLite database path")
	listen := flag.String("listen", "127.0.0.1:7777", "HTTP API address (empty to disable)")
	flushEvery := flag.Duration("flush", 10*time.Second, "how often to write counts to the database")
	flag.Parse()
	log.SetFlags(0) // journald adds timestamps

	store, err := OpenStore(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	counter := NewCounter()

	go watchKeyboards(counter, 5*time.Second)

	// Failed batches stay in memory and are retried every tick; they do not
	// survive the process ending (see README).
	failing := false
	flush := func() {
		n, err := counter.Flush(store.Add)
		switch {
		case err != nil:
			failing = true
			log.Printf("flush failed, keeping %d minutes in memory to retry: %v", n, err)
		case failing:
			failing = false
			log.Printf("flush recovered, saved %d minutes", n)
		}
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(*flushEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				flush()
			case <-stop:
				return
			}
		}
	}()

	var srv *http.Server
	if *listen != "" {
		srv = &http.Server{
			Addr:              *listen,
			Handler:           apiHandler(store, counter),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      requestTimeout + 5*time.Second,
			IdleTimeout:       60 * time.Second,
		}
		go func() {
			log.Printf("API on http://%s", *listen)
			if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				log.Fatal(err)
			}
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	// Order: stop serving, stop the timer flush, stop counting, final flush, close the db.
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		srv.Shutdown(ctx)
		cancel()
	}
	close(stop)
	<-stopped
	counter.Stop()
	if n, err := counter.Flush(store.Add); err != nil {
		log.Printf("final flush failed, %d minutes of counts lost: %v", n, err)
	}
	store.Close()
}

type Stats struct {
	Bucket
	AvgIntervalMs float64 `json:"avg_interval_ms"`
}

func withAvg(b Bucket) Stats {
	s := Stats{Bucket: b}
	if b.IntervalCount > 0 {
		s.AvgIntervalMs = float64(b.IntervalSumMs) / float64(b.IntervalCount)
	}
	return s
}

const (
	requestTimeout = 10 * time.Second
	maxQueued      = 16 // API requests running or waiting at once
)

// Host names the portal answers to. Anything else is refused, which blocks
// DNS-rebinding pages from reading the API through the browser.
var allowedHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "mashedkeys.local": true}

func hostAllowed(h string) bool {
	if host, port, err := net.SplitHostPort(h); err == nil {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return false
		}
		h = host
	} else if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	return allowedHosts[strings.ToLower(h)]
}

// Longest range per series request; the dashboard asks for at most ~15 months.
var maxRange = map[string]int64{
	"minute": 2 * 86400,
	"hour":   400 * 86400,
	"day":    3700 * 86400,
	"month":  36500 * 86400,
}

func apiHandler(store *Store, counter *Counter) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, webFS, "web/index.html")
	})
	mux.HandleFunc("GET /app.js", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, webFS, "web/app.js")
	})

	// Totals since boot, since local midnight, and all time (unflushed counts included).
	mux.HandleFunc("GET /api/summary", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		boot, err := bootTime()
		if err != nil {
			httpError(w, err)
			return
		}
		y, m, d := now.Date()
		names := []string{"all_time", "since_boot", "today"}
		froms := []int64{0, boot.Unix(), time.Date(y, m, d, 0, 0, 0, 0, time.Local).Unix()}
		out := map[string]any{"boot_time": boot, "now": now}
		// No flush runs while we read, and memory is read once, so all ranges agree.
		err = counter.Settled(r.Context(), func() error {
			p := counter.Pending(froms...)
			for i, from := range froms {
				b := store.Total()
				if from > 0 {
					if b, err = store.Sum(r.Context(), from, now.Unix()+60); err != nil {
						return err
					}
				}
				b.add(&p[i])
				out[names[i]] = withAvg(b)
			}
			return nil
		})
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, out)
	})

	// Grouped history: /api/series?period=hour|day|minute|month&from=<unix>&to=<unix>
	// Defaults to the last 7 days. Only flushed data is included.
	mux.HandleFunc("GET /api/series", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		period := q.Get("period")
		if period == "" {
			period = "day"
		}
		limit, ok := maxRange[period]
		if !ok {
			http.Error(w, "period must be minute, hour, day or month", http.StatusBadRequest)
			return
		}
		to, err1 := parseInt(q.Get("to"), time.Now().Unix()+60)
		from, err2 := parseInt(q.Get("from"), to-7*86400)
		if err := errors.Join(err1, err2); err != nil || from < 0 || to <= from || to-from > limit {
			http.Error(w, "from/to must be unix seconds with from < to, at most "+
				strconv.FormatInt(limit/86400, 10)+" days apart for "+period, http.StatusBadRequest)
			return
		}
		points, err := store.Series(r.Context(), period, from, to)
		if err != nil {
			httpError(w, err)
			return
		}
		out := make([]map[string]any, len(points))
		for i, p := range points {
			out[i] = map[string]any{"period": p.Period, "stats": withAvg(p.Bucket)}
		}
		writeJSON(w, out)
	})

	// At most maxQueued API requests run or wait; the rest get 503 at once. This
	// sits inside the timeout so a slot is held until the handler really returns.
	admit := make(chan struct{}, maxQueued)
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			select {
			case admit <- struct{}{}:
				defer func() { <-admit }()
			default:
				w.Header().Set("Retry-After", "1")
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
	timed := http.TimeoutHandler(api, requestTimeout, "request timed out")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		// style-src needs inline: the page has a <style> block and the charts set style attributes.
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
			"connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		if !hostAllowed(r.Host) {
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		timed.ServeHTTP(w, r)
	})
}

func bootTime() (time.Time, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			s, err := strconv.ParseInt(v, 10, 64)
			return time.Unix(s, 0), err
		}
	}
	return time.Time{}, errors.New("btime not found in /proc/stat")
}

// parseInt returns def for an empty string and an error for anything not an integer.
func parseInt(s string, def int64) (int64, error) {
	if s == "" {
		return def, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func httpError(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
