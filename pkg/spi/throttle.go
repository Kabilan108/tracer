package spi

import (
	"log/slog"
	"sync"
	"time"
)

// FileThrottle coalesces bursts of change events per key (normally a session
// file path) into one run per window, executed off the caller's goroutine. The
// first change schedules a run after the key's delay; later changes before it
// fires join that run, so latency is bounded by the delay even under a steady
// stream of writes.
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

// One second keeps archives close to live while absorbing the many small
// appends of a single agent turn. A cost factor of four caps a continuously
// growing file at roughly a fifth of one core.
const (
	sessionWatchMinDelay   = time.Second
	sessionWatchCostFactor = 4
)

// NewSessionFileThrottle returns the throttle provider watchers use to process
// changed session files, running at most maxConcurrent parses at once.
func NewSessionFileThrottle(maxConcurrent int, run func(path string)) *FileThrottle {
	return NewFileThrottle(sessionWatchMinDelay, sessionWatchCostFactor, maxConcurrent, run)
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

// Stop stops accepting changes, then processes every change already accepted
// without waiting out its delay, and returns once all runs have finished.
//
// Why drain instead of cancel: a watcher stops on Ctrl+C or service shutdown,
// and the last write of a session usually lands moments before that. Dropping
// scheduled runs would leave the archive stale until the next ingest. The
// drain is bounded: at most one run per changed key, plus one follow-up for a
// key that changed during its in-flight run.
func (t *FileThrottle) Stop() {
	t.mu.Lock()
	t.stopped = true
	for key, entry := range t.entries {
		// A false Stop means the timer already fired and its run is under way.
		if entry.timer != nil && entry.timer.Stop() {
			go t.fire(key)
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

// fire runs a scheduled key. Each call balances one pending.Add made when the
// run was scheduled.
func (t *FileThrottle) fire(key string) {
	defer t.pending.Done()

	t.mu.Lock()
	entry := t.entries[key]
	entry.timer = nil
	entry.running = true
	t.mu.Unlock()

	for again := true; again; {
		elapsed := t.runInSlot(key)

		t.mu.Lock()
		entry.lastDuration = elapsed
		if entry.dirty && !t.stopped {
			t.scheduleLocked(key, entry)
		}
		// While stopping, a change that landed during the run is processed
		// right away so Stop's caller sees it before shutting down.
		again = entry.dirty && t.stopped
		entry.dirty = false
		entry.running = again
		t.mu.Unlock()
	}
	// Idle entries are kept: a file written every few seconds would otherwise
	// lose its last duration between bursts and fall back to minDelay. Each
	// entry is a few dozen bytes and there is one per session file changed
	// while watching, so the map stays small.
}

func (t *FileThrottle) runInSlot(key string) time.Duration {
	t.slots <- struct{}{}
	defer func() { <-t.slots }()

	started := time.Now()
	t.runRecovered(key)
	return time.Since(started)
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
