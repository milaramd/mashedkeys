package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func get(t *testing.T, h http.Handler, host, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Host = host
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHostGuard(t *testing.T) {
	h := apiHandler(testStore(t), NewCounter())
	hosts := map[string]bool{
		"localhost": true, "LOCALHOST:80": true, "127.0.0.1": true, "127.0.0.1:7777": true,
		"[::1]": true, "[::1]:80": true, "mashedkeys.local": true, "mashedkeys.local:80": true,
		"attacker.example": false, "localhost.attacker.example": false, "mashedkeys.local.evil": false,
		"evil-localhost": false, "127.0.0.2": false, "localhost:abc": false, "localhost:99999": false,
		"": false, "[::1": false, "localhost.": false,
	}
	for _, path := range []string{"/", "/app.js", "/api/summary", "/api/series?period=day", "/nope"} {
		for host, ok := range hosts {
			code := get(t, h, host, path).Code
			if blocked := code == http.StatusMisdirectedRequest; blocked == ok {
				t.Errorf("Host %q %s: status %d", host, path, code)
			}
		}
	}
}

func TestHeaders(t *testing.T) {
	h := apiHandler(testStore(t), NewCounter())
	for _, path := range []string{"/", "/api/summary", "/api/series"} {
		w := get(t, h, "localhost", path)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		for k, want := range map[string]string{"Cache-Control": "no-store", "X-Frame-Options": "DENY"} {
			if got := w.Header().Get(k); got != want {
				t.Errorf("%s %s = %q", path, k, got)
			}
		}
		if w.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s: no CSP", path)
		}
	}
}

func TestSeriesValidation(t *testing.T) {
	h := apiHandler(testStore(t), NewCounter())
	now := time.Now().Unix()
	q := func(period string, from, to int64) string {
		return "/api/series?period=" + period + "&from=" + strconv.FormatInt(from, 10) + "&to=" + strconv.FormatInt(to, 10)
	}
	for path, want := range map[string]int{
		"/api/series":                          200,
		q("day", now-449*86400, now):           200, // 1-year view incl. heatmap weeks
		q("hour", now-366*86400, now):          200,
		q("minute", now-3600, now):             200,
		q("minute", now-3*86400, now):          400,
		q("hour", now-500*86400, now):          400,
		q("day", now, now-86400):               400,
		q("day", now, now):                     400,
		q("day", -5, now):                      400,
		"/api/series?period=year":              400,
		"/api/series?from=abc":                 400,
		"/api/series?to=99999999999999999999":  400,
		"/api/series?from=1.5&to=10":           400,
		"/api/series?period=day&from=0&to=600": 200,
	} {
		if got := get(t, h, "localhost", path).Code; got != want {
			t.Errorf("%s: %d, want %d", path, got, want)
		}
	}
}

func TestCancelledQuery(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Series(ctx, "day", 0, time.Now().Unix()); err == nil {
		t.Fatal("cancelled query succeeded")
	}
}

// A failed write keeps the batch; the next successful flush saves it exactly once,
// together with counts that arrived while writes were failing.
func TestFlushRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCounter()
	t0 := time.Unix(1_700_000_000, 0)
	press := func(n int, at time.Time) {
		for range n {
			c.Event(kA, 1, at)
		}
	}
	press(5, t0)
	fail := func(map[int64]*Bucket) error { return errors.New("disk full") }
	for range 3 {
		if _, err := c.Flush(fail); err == nil {
			t.Fatal("want error")
		}
	}
	press(2, t0)                  // same minute as the failed batch
	press(4, t0.Add(time.Minute)) // a new minute
	if p := c.Pending(0)[0]; p.Keys != 11 {
		t.Fatalf("pending during failure = %d, want 11", p.Keys)
	}
	if _, err := c.Flush(s.Add); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Flush(s.Add); err != nil { // nothing left: must not re-add
		t.Fatal(err)
	}
	got, err := s.Sum(context.Background(), 0, t0.Unix()+3600)
	if err != nil {
		t.Fatal(err)
	}
	if got.Keys != 11 || s.Total().Keys != 11 || c.Pending(0)[0].Keys != 0 {
		t.Fatalf("db %d, total %d, pending %d; want 11, 11, 0", got.Keys, s.Total().Keys, c.Pending(0)[0].Keys)
	}
	// Total survives a restart via the table scan.
	s.Close()
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.Total().Keys != 11 {
		t.Fatalf("total after reopen = %d", s2.Total().Keys)
	}
}

// Summaries read while counts move from memory to the store never miss any.
func TestSummaryDuringFlush(t *testing.T) {
	s, c := testStore(t), NewCounter()
	h := apiHandler(s, c)
	const n = 200
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range n {
			c.Event(kA, 1, time.Now())
			if i%3 == 0 {
				c.Flush(func(map[int64]*Bucket) error { return errors.New("flaky") })
			} else {
				c.Flush(s.Add)
			}
		}
	}()
	seen := int64(0)
	for done := false; !done; {
		done = c.Pending(0)[0].Keys == 0 && s.Total().Keys == n
		var out struct {
			AllTime Stats `json:"all_time"`
			Today   Stats
		}
		if err := json.Unmarshal(get(t, h, "localhost", "/api/summary").Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		all, today := out.AllTime.Keys, out.Today.Keys
		if all < seen || today != all {
			t.Fatalf("all_time %d (previously %d), today %d", all, seen, today)
		}
		seen = all
	}
	wg.Wait()
}

// Requests stuck behind a flush give up when cancelled and free their slot;
// requests beyond the admission limit are refused instead of queued.
func TestBusyRequestsReleased(t *testing.T) {
	s, c := testStore(t), NewCounter()
	h := apiHandler(s, c)
	release := make(chan struct{})
	defer close(release)
	go c.Flush(func(map[int64]*Bucket) error { <-release; return nil })
	for len(c.persist) == 0 {
		time.Sleep(time.Millisecond)
	}
	summary := func(ctx context.Context) string {
		r := httptest.NewRequest("GET", "/api/summary", nil).WithContext(ctx)
		r.Host = "localhost"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Body.String()
	}
	quick := func() string {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		return summary(ctx)
	}
	waitFor := func(busy bool) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); (quick() == "busy\n") != busy; {
			if time.Now().After(end) {
				t.Fatalf("never saw busy=%v", busy)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	for range maxQueued {
		go summary(ctx)
	}
	waitFor(true)  // all slots taken by requests waiting on the flush
	cancel()       // their clients go away...
	waitFor(false) // ...and the handlers return, freeing the slots
}

func TestStopBeforeFinalFlush(t *testing.T) {
	s, c := testStore(t), NewCounter()
	c.Event(kA, 1, time.Now())
	c.Stop()
	c.Event(kA, 1, time.Now()) // arrives during shutdown: ignored, not stranded
	if _, err := c.Flush(s.Add); err != nil {
		t.Fatal(err)
	}
	if s.Total().Keys != 1 || c.Pending(0)[0].Keys != 0 {
		t.Fatalf("saved %d, pending %d; want 1, 0", s.Total().Keys, c.Pending(0)[0].Keys)
	}
}
