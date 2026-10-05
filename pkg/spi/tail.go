package spi

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

// FileStat is the identity and size of a provider input file at one point in time.
type FileStat struct {
	Path    string
	Size    int64
	ModTime int64 // Unix nanoseconds
	Dev     uint64
	Inode   uint64
}

// StatFile returns the FileStat for path, following symlinks.
func StatFile(path string) (FileStat, error) {
	info, err := os.Stat(path)
	if err != nil {
		return FileStat{}, err
	}
	return FileStatFromInfo(path, info), nil
}

// FileStatFromInfo builds a FileStat from an existing os.FileInfo.
func FileStatFromInfo(path string, info os.FileInfo) FileStat {
	stat := FileStat{
		Path:    path,
		Size:    info.Size(),
		ModTime: info.ModTime().UnixNano(),
	}
	// Why: device+inode is what tells an append apart from a rename-over or a
	// rotation that happens to leave a larger file at the same path.
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		stat.Dev = uint64(sys.Dev)
		stat.Inode = uint64(sys.Ino)
	}
	return stat
}

// TailCursor records how much of an append-only file has been consumed.
// The zero value means nothing has been read yet.
type TailCursor struct {
	Dev    uint64
	Inode  uint64
	Offset int64 // bytes consumed; always just past a newline
}

// TailResult reports what one ReadAppendedLines call did.
type TailResult struct {
	Restarted bool  // the cursor no longer applied, so the file was read from the start
	Bytes     int64 // bytes of complete lines consumed by this call, newlines included
}

// ReadAppendedLines calls onLine for every complete line written to path after
// cursor, then advances cursor past the last newline.
//
// A trailing line without a newline is left unread: the writer may still be in
// the middle of it, and consuming half a JSON record would either drop it as
// corrupt or require re-reading it later anyway.
//
// When the file was truncated, replaced (different device/inode), or rewritten
// in place so the byte before the cursor is no longer a newline, restart is
// called before any line is delivered and the file is read from the beginning.
// Callers must discard their per-file state in restart.
//
// If onLine returns an error, reading stops, cursor is left unchanged, and the
// caller's per-file state reflects a partial read; callers must discard it.
func ReadAppendedLines(path string, cursor *TailCursor, restart func(), onLine func(line []byte) error) (TailResult, error) {
	var result TailResult

	file, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return result, fmt.Errorf("stat %s: %w", path, err)
	}
	stat := FileStatFromInfo(path, info)

	start := cursor.Offset
	if !cursorContinues(file, *cursor, stat) {
		start = 0
		result.Restarted = true
		restart()
	}

	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return result, fmt.Errorf("seek %s: %w", path, err)
	}

	reader := bufio.NewReaderSize(file, 64*1024)
	offset := start
	for {
		line, readErr := reader.ReadBytes('\n')
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return result, fmt.Errorf("read %s at offset %d: %w", path, offset, readErr)
		}
		offset += int64(len(line))
		if err := onLine(line[:len(line)-1]); err != nil {
			return result, err
		}
	}

	*cursor = TailCursor{Dev: stat.Dev, Inode: stat.Inode, Offset: offset}
	result.Bytes = offset - start
	return result, nil
}

// cursorContinues reports whether the file can be read from cursor.Offset.
func cursorContinues(file *os.File, cursor TailCursor, stat FileStat) bool {
	if cursor.Inode == 0 && cursor.Offset == 0 {
		return false
	}
	if cursor.Dev != stat.Dev || cursor.Inode != stat.Inode || stat.Size < cursor.Offset {
		return false
	}
	if cursor.Offset == 0 {
		return true
	}
	// Why: a truncate-and-rewrite between two events can leave the file at
	// least as long as before with the same inode; the consumed prefix ending
	// in a newline is the cheapest evidence that it is still the same content.
	var last [1]byte
	if _, err := file.ReadAt(last[:], cursor.Offset-1); err != nil {
		return false
	}
	return last[0] == '\n'
}

// DefaultTailIdleTTL is how long a session file's decoded records stay cached
// after the file was last read.
// Why 10 minutes: an active agent appends to its transcript every few seconds
// to minutes (streamed turns, tool calls), so live sessions stay cached. A file
// quiet for longer is most likely finished; if it resumes, the cost is one full
// re-parse, which is what every write event cost before incremental reading.
// Decoded records take several times the file size in memory, so holding
// finished sessions indefinitely is not worth it.
const DefaultTailIdleTTL = 10 * time.Minute

// TailStates holds per-file incremental read state keyed by path and drops
// entries that have not been used for longer than an idle TTL.
type TailStates[S any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]*tailEntry[S]
	timer   *time.Timer
}

type tailEntry[S any] struct {
	state    S
	lastUsed time.Time
}

// NewTailStates creates an empty state map whose entries expire after ttl of disuse.
func NewTailStates[S any](ttl time.Duration) *TailStates[S] {
	return &TailStates[S]{
		ttl:     ttl,
		entries: make(map[string]*tailEntry[S]),
	}
}

// Get returns the state for path and marks it as recently used.
func (t *TailStates[S]) Get(path string) (S, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	entry, ok := t.entries[path]
	if !ok {
		var zero S
		return zero, false
	}
	entry.lastUsed = time.Now()
	return entry.state, true
}

// Put stores the state for path and marks it as recently used.
func (t *TailStates[S]) Put(path string, state S) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.entries[path] = &tailEntry[S]{state: state, lastUsed: time.Now()}
	t.scheduleSweepLocked()
}

// Forget drops the state for path, e.g. after the file was removed.
func (t *TailStates[S]) Forget(path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, path)
}

// Len returns the number of cached entries.
func (t *TailStates[S]) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

// EvictIdle drops every entry last used before now minus the TTL.
func (t *TailStates[S]) EvictIdle(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for path, entry := range t.entries {
		if now.Sub(entry.lastUsed) > t.ttl {
			delete(t.entries, path)
		}
	}
}

// scheduleSweepLocked arms a single timer while entries exist.
// Why a timer instead of sweeping on access: an agent that stops writing
// produces no further events, so access-driven eviction would keep its
// decoded records resident indefinitely.
func (t *TailStates[S]) scheduleSweepLocked() {
	if t.timer != nil || len(t.entries) == 0 {
		return
	}
	t.timer = time.AfterFunc(t.ttl, func() {
		t.EvictIdle(time.Now())
		t.mu.Lock()
		t.timer = nil
		t.scheduleSweepLocked()
		t.mu.Unlock()
	})
}
