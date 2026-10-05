package spi

import (
	"log/slog"
	"sync"
	"time"
)

// FileThrottle coalesces bursts of change events per key (normally a session
// file path) into trailing-edge runs executed off the caller's goroutine.
//
// Why: agents append to transcripts many times per second, and each run of a
// provider parser re-reads the whole file. Running the parser once per
// fsnotify event made watcher CPU scale with events x file size, and doing it
// inline in the event loop let events queue up behind slow parses so the
// backlog was replayed as back-to-back full parses.
type FileThrottle struct {
	minDelay   time.Duration
	costFactor time.Duration
	run        func(key string)
	slots      chan struct{}

	mu      sync.Mutex
	entries map[string]*throttleEntry
	stopped bool
	pending sync.WaitGroup
}

// Session watchers share these settings. One second keeps archives close to
// live while absorbing the many small appends of a single agent turn. A cost
// factor of four caps a continuously growing file at roughly a fifth of one
// core. Two concurrent runs let a small session update while a large one is
// being parsed, without holding several large parses in memory at once.
const (
	sessionWatchMinDelay      = time.Second
	sessionWatchCostFactor    = 4
	sessionWatchMaxConcurrent = 2
)

// NewSessionFileThrottle returns the throttle provider watchers use to process
// changed session files.
func NewSessionFileThrottle(run func(path string)) *FileThrottle {
	return NewFileThrottle(sessionWatchMinDelay, sessionWatchCostFactor, sessionWatchMaxConcurrent, run)
}

type throttleEntry struct {
	timer        *time.Timer
	running      bool
	dirty        bool
	lastDuration time.Duration
}

// NewFileThrottle returns a throttle that runs run(key) at most once per
// delay after a Trigger. The delay is the larger of minDelay and costFactor
// times the previous run's duration for that key, so a file that takes long
// to process is processed proportionally less often while it keeps changing.
// At most maxConcurrent runs execute at once across all keys, bounding the
// memory held by simultaneous parses of large files.
func NewFileThrottle(minDelay time.Duration, costFactor int, maxConcurrent int, run func(key string)) *FileThrottle {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &FileThrottle{
		minDelay:   minDelay,
		costFactor: time.Duration(costFactor),
		run:        run,
		slots:      make(chan struct{}, maxConcurrent),
		entries:    make(map[string]*throttleEntry),
	}
}

// Trigger records that key changed. It never blocks on a run.
func (t *FileThrottle) Trigger(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.stopped {
		return
	}
	entry, ok := t.entries[key]
	if !ok {
		entry = &throttleEntry{}
		t.entries[key] = entry
	}
	switch {
	case entry.running:
		// The in-flight run may have read the file before this change landed,
		// so schedule exactly one follow-up when it finishes.
		entry.dirty = true
	case entry.timer == nil:
		t.scheduleLocked(key, entry)
	}
}

// Stop cancels scheduled runs and waits for in-flight runs to return, so no
// run starts or finishes after Stop returns.
func (t *FileThrottle) Stop() {
	t.mu.Lock()
	t.stopped = true
	for _, entry := range t.entries {
		if entry.timer != nil && entry.timer.Stop() {
			entry.timer = nil
			t.pending.Done()
		}
	}
	t.mu.Unlock()

	t.pending.Wait()
}

func (t *FileThrottle) scheduleLocked(key string, entry *throttleEntry) {
	delay := max(t.minDelay, t.costFactor*entry.lastDuration)
	t.pending.Add(1)
	entry.timer = time.AfterFunc(delay, func() { t.fire(key) })
}

func (t *FileThrottle) fire(key string) {
	defer t.pending.Done()

	t.mu.Lock()
	entry := t.entries[key]
	entry.timer = nil
	if t.stopped {
		t.mu.Unlock()
		return
	}
	entry.running = true
	t.mu.Unlock()

	t.slots <- struct{}{}
	started := time.Now()
	t.runRecovered(key)
	elapsed := time.Since(started)
	<-t.slots

	t.mu.Lock()
	defer t.mu.Unlock()
	entry.running = false
	entry.lastDuration = elapsed
	// Idle entries are kept: a file written every few seconds would otherwise
	// lose its last duration between bursts and fall back to minDelay. Each
	// entry is a few dozen bytes and there is one per session file ever
	// changed while watching, so the map stays small.
	if entry.dirty && !t.stopped {
		entry.dirty = false
		t.scheduleLocked(key, entry)
	}
}

// runRecovered isolates the throttle's bookkeeping from a panicking run, which
// would otherwise leave the key marked running forever.
func (t *FileThrottle) runRecovered(key string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("FileThrottle: run panicked", "key", key, "panic", r)
		}
	}()
	t.run(key)
}
