package main

import (
	"testing"
	"time"
)

// Key codes for the test sequence.
const (
	kH, kI, kA, kC = 35, 23, 30, 46
)

func TestCounter(t *testing.T) {
	c := NewCounter()
	t0 := time.Unix(1_700_000_000, 0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	press := func(code uint16, ms int) {
		c.Event(code, 1, at(ms))
		c.Event(code, 0, at(ms+10))
	}

	press(kH, 0)
	press(kI, 150)
	press(keySpace, 300) // word 1: "hi"
	press(keySpace, 400) // double space: no word
	press(kA, 3400)      // after a 3s pause
	press(keyBackspace, 3500)
	press(keySpace, 3600) // "a" was erased: no word
	c.Event(keyLeftCtrl, 1, at(3700))
	press(kC, 3750) // ctrl+c: not a character
	c.Event(keyLeftCtrl, 0, at(3800))
	press(keyEnter, 3900) // nothing typed: no word
	press(kA, 4000)
	c.Event(kA, 2, at(4050)) // autorepeat: ignored
	press(keyEnter, 4100)    // word 2

	var b Bucket
	for _, x := range c.Drain() {
		b.add(x)
	}
	if b.Keys != 12 || b.Words != 2 || b.Backspaces != 1 || b.Enters != 2 {
		t.Fatalf("got %+v", b)
	}
	if b.Hist[5] != 1 { // the one 3s pause
		t.Fatalf("pause bucket = %d, want 1", b.Hist[5])
	}
	if b.IntervalCount != 10 {
		t.Fatalf("interval count = %d, want 10", b.IntervalCount)
	}
}
