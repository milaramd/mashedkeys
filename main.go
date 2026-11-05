// mashedkeys counts how many keys are pressed (never which ones) and
// stores per-minute aggregates in SQLite, served as JSON over HTTP.
package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"log"
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

	flush := func() {
		if err := store.Add(counter.Drain()); err != nil {
			log.Printf("flush: %v", err)
		}
	}
	go func() {
		for range time.Tick(*flushEvery) {
			flush()
		}
	}()

	if *listen != "" {
		go func() {
			log.Printf("API on http://%s", *listen)
			log.Fatal(http.ListenAndServe(*listen, apiHandler(store, counter)))
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	flush()
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

func apiHandler(store *Store, counter *Counter) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, webFS, "web/index.html")
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
		ranges := map[string]int64{
			"since_boot": boot.Unix(),
			"today":      time.Date(y, m, d, 0, 0, 0, 0, time.Local).Unix(),
			"all_time":   0,
		}
		out := map[string]any{"boot_time": boot, "now": now}
		for name, from := range ranges {
			b, err := store.Sum(from, now.Unix()+60)
			if err != nil {
				httpError(w, err)
				return
			}
			p := counter.Pending(from)
			b.add(&p)
			out[name] = withAvg(b)
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
		to := parseInt(q.Get("to"), time.Now().Unix()+60)
		from := parseInt(q.Get("from"), to-7*86400)
		points, err := store.Series(period, from, to)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out := make([]map[string]any, len(points))
		for i, p := range points {
			out[i] = map[string]any{"period": p.Period, "stats": withAvg(p.Bucket)}
		}
		writeJSON(w, out)
	})
	return mux
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

func parseInt(s string, def int64) int64 {
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		return v
	}
	return def
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
