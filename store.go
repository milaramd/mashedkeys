package main

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS minutes (
	ts              INTEGER PRIMARY KEY, -- unix seconds, start of the minute
	keys            INTEGER NOT NULL DEFAULT 0,
	words           INTEGER NOT NULL DEFAULT 0,
	backspaces      INTEGER NOT NULL DEFAULT 0,
	deletes         INTEGER NOT NULL DEFAULT 0,
	enters          INTEGER NOT NULL DEFAULT 0,
	interval_sum_ms INTEGER NOT NULL DEFAULT 0,
	interval_count  INTEGER NOT NULL DEFAULT 0,
	h0 INTEGER NOT NULL DEFAULT 0, h1 INTEGER NOT NULL DEFAULT 0, h2 INTEGER NOT NULL DEFAULT 0,
	h3 INTEGER NOT NULL DEFAULT 0, h4 INTEGER NOT NULL DEFAULT 0, h5 INTEGER NOT NULL DEFAULT 0
);`

const sumCols = `COALESCE(SUM(keys),0), COALESCE(SUM(words),0), COALESCE(SUM(backspaces),0),
	COALESCE(SUM(deletes),0), COALESCE(SUM(enters),0), COALESCE(SUM(interval_sum_ms),0),
	COALESCE(SUM(interval_count),0), COALESCE(SUM(h0),0), COALESCE(SUM(h1),0), COALESCE(SUM(h2),0),
	COALESCE(SUM(h3),0), COALESCE(SUM(h4),0), COALESCE(SUM(h5),0)`

// MaxReaders bounds concurrent API queries; the pool keeps one more
// connection so the writer is never starved by readers (WAL lets them overlap).
const MaxReaders = 4

type Store struct {
	db      *sql.DB
	readers chan struct{}
	mu      sync.Mutex
	total   Bucket // all-time totals, kept in step with committed writes
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(MaxReaders + 1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, readers: make(chan struct{}, MaxReaders)}
	if s.total, err = scanBucket(db.QueryRow(`SELECT ` + sumCols + ` FROM minutes`)); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Total returns all-time totals without scanning the table.
func (s *Store) Total() Bucket {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// read runs a query once a reader slot is free; waiting is bounded by ctx.
func (s *Store) read(ctx context.Context, f func() error) error {
	select {
	case s.readers <- struct{}{}:
		defer func() { <-s.readers }()
		return f()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) Close() error { return s.db.Close() }

// Add merges buckets into the table, adding to any existing row.
func (s *Store) Add(buckets map[int64]*Bucket) error {
	if len(buckets) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO minutes VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(ts) DO UPDATE SET
			keys=keys+excluded.keys, words=words+excluded.words,
			backspaces=backspaces+excluded.backspaces, deletes=deletes+excluded.deletes,
			enters=enters+excluded.enters, interval_sum_ms=interval_sum_ms+excluded.interval_sum_ms,
			interval_count=interval_count+excluded.interval_count,
			h0=h0+excluded.h0, h1=h1+excluded.h1, h2=h2+excluded.h2,
			h3=h3+excluded.h3, h4=h4+excluded.h4, h5=h5+excluded.h5`)
	if err != nil {
		return err
	}
	for ts, b := range buckets {
		h := b.Hist
		if _, err := stmt.Exec(ts, b.Keys, b.Words, b.Backspaces, b.Deletes, b.Enters,
			b.IntervalSumMs, b.IntervalCount, h[0], h[1], h[2], h[3], h[4], h[5]); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	for _, b := range buckets {
		s.total.add(b)
	}
	s.mu.Unlock()
	return nil
}

func scanBucket(row interface{ Scan(...any) error }, extra ...any) (Bucket, error) {
	var b Bucket
	h := &b.Hist
	dest := append(extra, &b.Keys, &b.Words, &b.Backspaces, &b.Deletes, &b.Enters,
		&b.IntervalSumMs, &b.IntervalCount, &h[0], &h[1], &h[2], &h[3], &h[4], &h[5])
	return b, row.Scan(dest...)
}

// Sum totals all minutes in [from, to).
func (s *Store) Sum(ctx context.Context, from, to int64) (b Bucket, err error) {
	err = s.read(ctx, func() error {
		b, err = scanBucket(s.db.QueryRowContext(ctx, `SELECT `+sumCols+` FROM minutes WHERE ts >= ? AND ts < ?`, from/60*60, to))
		return err
	})
	return b, err
}

type SeriesPoint struct {
	Period string `json:"period"`
	Bucket
}

var periodFormats = map[string]string{
	"minute": "%Y-%m-%d %H:%M",
	"hour":   "%Y-%m-%d %H:00",
	"day":    "%Y-%m-%d",
	"month":  "%Y-%m",
}

// Series groups minutes in [from, to) by local-time period.
func (s *Store) Series(ctx context.Context, period string, from, to int64) (out []SeriesPoint, err error) {
	f, ok := periodFormats[period]
	if !ok {
		return nil, fmt.Errorf("unknown period %q", period)
	}
	err = s.read(ctx, func() error {
		out, err = s.series(ctx, f, from, to)
		return err
	})
	return out, err
}

func (s *Store) series(ctx context.Context, f string, from, to int64) ([]SeriesPoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT strftime(?, ts, 'unixepoch', 'localtime') AS p, `+sumCols+`
		FROM minutes WHERE ts >= ? AND ts < ? GROUP BY p ORDER BY p`, f, from/60*60, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SeriesPoint{}
	for rows.Next() {
		var p SeriesPoint
		p.Bucket, err = scanBucket(rows, &p.Period)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
