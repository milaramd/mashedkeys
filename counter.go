package main

import (
	"context"
	"sync"
	"time"
)

// Linux key codes (linux/input-event-codes.h) we care about.
const (
	keyBackspace = 14
	keyTab       = 15
	keyEnter     = 28
	keyLeftCtrl  = 29
	keyLeftAlt   = 56
	keySpace     = 57
	keyKPEnter   = 96
	keyRightCtrl = 97
	keyDelete    = 111
	keyLeftMeta  = 125
	keyRightMeta = 126
)

// Inter-key interval histogram edges in ms. The last bucket (>= 2000ms)
// counts pauses, which are left out of the average.
var histEdges = [...]int64{100, 200, 500, 1000, 2000}

const pauseMs = 2000

// Bucket holds aggregate counts for one minute. Nothing here records which
// key was pressed or in what order.
type Bucket struct {
	Keys          int64    `json:"keys"`
	Words         int64    `json:"words"`
	Backspaces    int64    `json:"backspaces"`
	Deletes       int64    `json:"deletes"`
	Enters        int64    `json:"enters"`
	IntervalSumMs int64    `json:"interval_sum_ms"`
	IntervalCount int64    `json:"interval_count"`
	Hist          [6]int64 `json:"hist"` // <100, <200, <500, <1000, <2000, >=2000 ms
}

func (b *Bucket) add(o *Bucket) {
	b.Keys += o.Keys
	b.Words += o.Words
	b.Backspaces += o.Backspaces
	b.Deletes += o.Deletes
	b.Enters += o.Enters
	b.IntervalSumMs += o.IntervalSumMs
	b.IntervalCount += o.IntervalCount
	for i := range b.Hist {
		b.Hist[i] += o.Hist[i]
	}
}

// Counter turns key events from all keyboards into per-minute buckets.
type Counter struct {
	mu      sync.Mutex
	buckets map[int64]*Bucket // unix minute start -> counts
	retry   map[int64]*Bucket // drained but not committed yet; kept until a flush succeeds
	// persist (capacity 1) is held while a batch moves into the store, and by
	// Settled readers so store totals and Pending agree. It's a channel so
	// readers can give up when their request is cancelled. Lock order: persist, then mu.
	persist   chan struct{}
	stopped   bool // set by Stop; later events are ignored
	last      time.Time
	wordChars int       // chars typed in the current word (only a number)
	held      [256]bool // modifiers currently down
}

func NewCounter() *Counter {
	return &Counter{buckets: map[int64]*Bucket{}, persist: make(chan struct{}, 1)}
}

// Event handles one EV_KEY event. value: 0 = release, 1 = press, 2 = autorepeat.
func (c *Counter) Event(code uint16, value int32, t time.Time) {
	if code >= 256 { // BTN_* range: mouse/gamepad buttons, not keys
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}

	if value != 1 {
		if value == 0 {
			c.held[code] = false
		}
		return
	}
	c.held[code] = true

	minute := t.Unix() / 60 * 60
	b := c.buckets[minute]
	if b == nil {
		b = &Bucket{}
		c.buckets[minute] = b
	}
	b.Keys++

	if !c.last.IsZero() {
		if d := t.Sub(c.last).Milliseconds(); d >= 0 {
			i := 0
			for i < len(histEdges) && d >= histEdges[i] {
				i++
			}
			b.Hist[i]++
			if d < pauseMs {
				b.IntervalSumMs += d
				b.IntervalCount++
			}
		}
	}
	c.last = t

	shortcut := c.held[keyLeftCtrl] || c.held[keyRightCtrl] || c.held[keyLeftAlt] ||
		c.held[keyLeftMeta] || c.held[keyRightMeta]

	switch {
	case code == keyBackspace:
		b.Backspaces++
		if shortcut { // ctrl+backspace deletes the whole word
			c.wordChars = 0
		} else if c.wordChars > 0 {
			c.wordChars--
		}
	case code == keyDelete:
		b.Deletes++
	case code == keySpace || code == keyTab || code == keyEnter || code == keyKPEnter:
		if code == keyEnter || code == keyKPEnter {
			b.Enters++
		}
		if c.wordChars > 0 {
			b.Words++
		}
		c.wordChars = 0
	case printable(code) && !shortcut:
		c.wordChars++
	}
}

// printable reports whether the key normally produces a character on a
// standard layout (letters, digits, punctuation, keypad digits).
func printable(code uint16) bool {
	switch {
	case code >= 2 && code <= 13, // 1..0 - =
		code >= 16 && code <= 27, // q..] row
		code >= 30 && code <= 41, // a..` row
		code >= 43 && code <= 53, // \ z.. /
		code == 55,               // KP *
		code >= 71 && code <= 83, // keypad
		code == 86,               // 102nd key (< > on ISO)
		code == 98:               // KP /
		return true
	}
	return false
}

// Flush moves all counts into the store via add, which must commit all or
// nothing. On error the batch is kept and retried (merged with newer counts)
// on the next call, so nothing is lost or counted twice. Calls are serialized.
func (c *Counter) Flush(add func(map[int64]*Bucket) error) (int, error) {
	c.persist <- struct{}{}
	defer func() { <-c.persist }()
	c.mu.Lock()
	if c.retry == nil {
		c.retry = c.buckets
	} else {
		for ts, b := range c.buckets {
			if r := c.retry[ts]; r != nil {
				r.add(b)
			} else {
				c.retry[ts] = b
			}
		}
	}
	c.buckets = map[int64]*Bucket{}
	batch := c.retry
	c.mu.Unlock()
	if err := add(batch); err != nil {
		return len(batch), err
	}
	c.mu.Lock()
	c.retry = nil
	c.mu.Unlock()
	return len(batch), nil
}

// Stop ignores all later events, so a final Flush saves everything counted.
func (c *Counter) Stop() {
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
}

// Settled runs f while no flush is in progress, giving up if ctx ends first.
func (c *Counter) Settled(ctx context.Context, f func() error) error {
	select {
	case c.persist <- struct{}{}:
		defer func() { <-c.persist }()
		return f()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Pending sums the unflushed counts (including failed batches) for buckets
// >= each since, all from one snapshot.
func (c *Counter) Pending(sinces ...int64) []Bucket {
	c.mu.Lock()
	defer c.mu.Unlock()
	sums := make([]Bucket, len(sinces))
	for _, m := range []map[int64]*Bucket{c.buckets, c.retry} {
		for ts, b := range m {
			for i, since := range sinces {
				if ts >= since/60*60 {
					sums[i].add(b)
				}
			}
		}
	}
	return sums
}
