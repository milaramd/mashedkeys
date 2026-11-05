package main

import (
	"bufio"
	"encoding/binary"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// inputEvent mirrors struct input_event on 64-bit Linux.
type inputEvent struct {
	Sec, Usec int64
	Type      uint16
	Code      uint16
	Value     int32
}

const (
	evKey = 1
	evRep = 20
)

// findKeyboards parses /proc/bus/input/devices and returns /dev/input/eventN
// paths for devices that look like real keyboards: a "kbd" handler and
// autorepeat support (power buttons, lid switches, mice don't have EV_REP).
func findKeyboards() (map[string]string, error) {
	f, err := os.Open("/proc/bus/input/devices")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	found := map[string]string{} // path -> name
	var name, event string
	var kbd, rep bool
	flush := func() {
		if kbd && rep && event != "" {
			found["/dev/input/"+event] = name
		}
		name, event, kbd, rep = "", "", false, false
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "N: Name="):
			name = strings.Trim(strings.TrimPrefix(line, "N: Name="), `"`)
		case strings.HasPrefix(line, "H: Handlers="):
			for _, h := range strings.Fields(strings.TrimPrefix(line, "H: Handlers=")) {
				if h == "kbd" {
					kbd = true
				} else if strings.HasPrefix(h, "event") {
					event = h
				}
			}
		case strings.HasPrefix(line, "B: EV="):
			bits, err := strconv.ParseUint(strings.TrimPrefix(line, "B: EV="), 16, 64)
			rep = err == nil && bits&(1<<evRep) != 0 && bits&(1<<evKey) != 0
		}
	}
	flush()
	return found, sc.Err()
}

// watchKeyboards rescans for keyboards every interval so hotplugged ones are
// picked up, and runs one reader goroutine per device.
func watchKeyboards(c *Counter, interval time.Duration) {
	var mu sync.Mutex
	open := map[string]bool{}
	for {
		devs, err := findKeyboards()
		if err != nil {
			log.Printf("scan input devices: %v", err)
		}
		for path, name := range devs {
			mu.Lock()
			already := open[path]
			open[path] = true
			mu.Unlock()
			if already {
				continue
			}
			go func() {
				log.Printf("reading %s (%s)", path, name)
				err := readDevice(path, c)
				log.Printf("stopped %s (%s): %v", path, name, err)
				mu.Lock()
				delete(open, path)
				mu.Unlock()
			}()
		}
		time.Sleep(interval)
	}
}

func readDevice(path string, c *Counter) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var ev inputEvent
	for {
		if err := binary.Read(r, binary.NativeEndian, &ev); err != nil {
			return err
		}
		if ev.Type == evKey {
			c.Event(ev.Code, ev.Value, time.Unix(ev.Sec, ev.Usec*1000))
		}
	}
}
