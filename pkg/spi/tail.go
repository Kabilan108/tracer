package spi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
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

	// Size and ModTime are the file's stat when it was last read, and
	// Boundary hashes the bytes just before Offset. Together they tell an
	// append from an in-place rewrite (see cursorContinues).
	Size     int64
	ModTime  int64
	Boundary uint64
}

// TailResult reports what one ReadAppendedLines call did.
type TailResult struct {
	Restarted bool  // the cursor no longer applied, so the file was read from the start
	Bytes     int64 // bytes of complete lines consumed by this call, newlines included

	// Pending is the text after the last newline when it is a complete JSON
	// value. It is not consumed: the cursor stays before it, so it is
	// delivered again, as a regular line, once its newline is written.
	// Callers include it in their output without adding it to their per-file
	// state, which keeps it from being counted twice.
	Pending []byte
}

// boundaryWindow is how many bytes before the cursor offset are hashed.
// Why 64: the end of a JSONL record usually carries its timestamp or ids, so
// the last bytes before the boundary differ between records; 64 bytes cost a
// single small pread to verify.
const boundaryWindow = 64

// ReadAppendedLines calls onLine for every complete line written to path after
// cursor, then advances cursor past the last newline.
//
// A trailing line without a newline is not consumed: the writer may still be in
// the middle of it, and consuming half a JSON record would either drop it as
// corrupt or require re-reading it later anyway. When that trailing text is
// already a complete JSON value it is returned in TailResult.Pending, because
// a full read of the file includes it as the final record.
//
// When the file was truncated, replaced (different device/inode), or rewritten
// in place (see cursorContinues), restart is called before any line is
// delivered and the file is read from the beginning. Callers must discard
// their per-file state in restart.
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
			if trailing := bytes.TrimSpace(line); len(trailing) > 0 && json.Valid(trailing) {
				result.Pending = line
			}
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

	boundary, err := boundaryHash(file, offset)
	if err != nil {
		return result, fmt.Errorf("read %s before offset %d: %w", path, offset, err)
	}
	*cursor = TailCursor{
		Dev:      stat.Dev,
		Inode:    stat.Inode,
		Offset:   offset,
		Size:     stat.Size,
		ModTime:  stat.ModTime,
		Boundary: boundary,
	}
	result.Bytes = offset - start
	return result, nil
}

// cursorContinues reports whether the file can be read from cursor.Offset.
//
// Why these checks: transcripts only grow, so an append always leaves the
// same device and inode with a larger size. A file that is smaller than at the
// last read, or the same size with a different mtime, was modified some other
// way. A rewrite that grows the file is caught by the hash of the bytes just
// before the consumed boundary no longer matching.
//
// Blind spot: a rewrite that grows the file (or keeps its size within the
// filesystem's mtime granularity) and leaves the boundaryWindow bytes before
// the offset identical is taken for an append, so changed earlier records stay
// cached until the file is replaced, truncated or evicted from the cache.
// Hashing more of the prefix would close it at the cost of re-reading it on
// every event, which is the cost incremental reading removes.
func cursorContinues(file *os.File, cursor TailCursor, stat FileStat) bool {
	if cursor.Inode == 0 && cursor.Offset == 0 {
		return false
	}
	if cursor.Dev != stat.Dev || cursor.Inode != stat.Inode || stat.Size < cursor.Offset {
		return false
	}
	if stat.Size < cursor.Size || (stat.Size == cursor.Size && stat.ModTime != cursor.ModTime) {
		return false
	}
	boundary, err := boundaryHash(file, cursor.Offset)
	return err == nil && boundary == cursor.Boundary
}

// boundaryHash hashes the up to boundaryWindow bytes that end at offset.
func boundaryHash(file *os.File, offset int64) (uint64, error) {
	size := min(offset, boundaryWindow)
	window := make([]byte, size)
	if _, err := file.ReadAt(window, offset-size); err != nil {
		return 0, err
	}
	hash := fnv.New64a()
	_, _ = hash.Write(window)
	return hash.Sum64(), nil
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
