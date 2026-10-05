package spi

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// tailReader accumulates lines the way provider caches do, dropping them on restart.
type tailReader struct {
	cursor   TailCursor
	lines    []string
	restarts int
}

func (r *tailReader) read(t *testing.T, path string) TailResult {
	t.Helper()
	result, err := ReadAppendedLines(path, &r.cursor,
		func() {
			r.restarts++
			r.lines = nil
		},
		func(line []byte) error {
			r.lines = append(r.lines, string(line))
			return nil
		})
	if err != nil {
		t.Fatalf("ReadAppendedLines() error = %v", err)
	}
	return result
}

func appendFile(t *testing.T, path string, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := file.WriteString(content); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestReadAppendedLines(t *testing.T) {
	tests := []struct {
		name string
		// initial content, read once before mutate
		initial string
		mutate  func(t *testing.T, path string)
		// expected outcome of the second read
		wantLines     []string
		wantRestarted bool
		wantBytes     int64
	}{
		{
			name:      "append reads only new complete lines",
			initial:   "a\nb\n",
			mutate:    func(t *testing.T, path string) { appendFile(t, path, "c\nd\n") },
			wantLines: []string{"a", "b", "c", "d"},
			wantBytes: 4,
		},
		{
			name:      "partial trailing line is left for the next read",
			initial:   "a\n",
			mutate:    func(t *testing.T, path string) { appendFile(t, path, "b\n{\"half\":") },
			wantLines: []string{"a", "b"},
			wantBytes: 2,
		},
		{
			name:      "empty lines are delivered so line numbers stay aligned",
			initial:   "a\n",
			mutate:    func(t *testing.T, path string) { appendFile(t, path, "\nb\n") },
			wantLines: []string{"a", "", "b"},
			wantBytes: 3,
		},
		{
			name:    "truncation restarts from the beginning",
			initial: "a\nb\nc\n",
			mutate: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantLines:     []string{"x"},
			wantRestarted: true,
			wantBytes:     2,
		},
		{
			name:    "replacement with a new inode restarts even when larger",
			initial: "a\n",
			mutate: func(t *testing.T, path string) {
				replacement := path + ".new"
				if err := os.WriteFile(replacement, []byte("x\ny\nz\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			},
			wantLines:     []string{"x", "y", "z"},
			wantRestarted: true,
			wantBytes:     6,
		},
		{
			name:    "in-place rewrite that moves the newline restarts",
			initial: "ab\n",
			mutate: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("abcd\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantLines:     []string{"abcd"},
			wantRestarted: true,
			wantBytes:     5,
		},
		{
			name:    "same-size rewrite with a different boundary restarts",
			initial: "a\nb\n",
			mutate: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("x\ny\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantLines:     []string{"x", "y"},
			wantRestarted: true,
			wantBytes:     4,
		},
		{
			// The bytes before the boundary are unchanged, so only the mtime of
			// a file that did not grow reveals the rewrite.
			name:    "same-size rewrite with a new mtime restarts",
			initial: "first\n" + strings.Repeat("z", 80) + "\n",
			mutate: func(t *testing.T, path string) {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("FIRST\n"+strings.Repeat("z", 80)+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				later := info.ModTime().Add(time.Second)
				if err := os.Chtimes(path, later, later); err != nil {
					t.Fatal(err)
				}
			},
			wantLines:     []string{"FIRST", strings.Repeat("z", 80)},
			wantRestarted: true,
			wantBytes:     87,
		},
		{
			name:    "growing rewrite that keeps the newline at the offset restarts",
			initial: "a\nb\n",
			mutate: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("x\ny\nz\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantLines:     []string{"x", "y", "z"},
			wantRestarted: true,
			wantBytes:     6,
		},
		{
			name:      "unchanged file reads nothing",
			initial:   "a\nb\n",
			mutate:    func(t *testing.T, path string) {},
			wantLines: []string{"a", "b"},
			wantBytes: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(path, []byte(tt.initial), 0o644); err != nil {
				t.Fatal(err)
			}

			reader := &tailReader{}
			first := reader.read(t, path)
			if !first.Restarted {
				t.Errorf("first read Restarted = false, want true")
			}

			tt.mutate(t, path)
			second := reader.read(t, path)

			if !reflect.DeepEqual(reader.lines, tt.wantLines) {
				t.Errorf("lines = %q, want %q", reader.lines, tt.wantLines)
			}
			if second.Restarted != tt.wantRestarted {
				t.Errorf("Restarted = %v, want %v", second.Restarted, tt.wantRestarted)
			}
			if second.Bytes != tt.wantBytes {
				t.Errorf("Bytes = %d, want %d", second.Bytes, tt.wantBytes)
			}
		})
	}
}

func TestReadAppendedLines_CompletesPartialLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte("a\n{\"half\":"), 0o644); err != nil {
		t.Fatal(err)
	}

	reader := &tailReader{}
	reader.read(t, path)
	if want := []string{"a"}; !reflect.DeepEqual(reader.lines, want) {
		t.Fatalf("lines before completion = %q, want %q", reader.lines, want)
	}

	appendFile(t, path, "1}\n")
	result := reader.read(t, path)
	if want := []string{"a", `{"half":1}`}; !reflect.DeepEqual(reader.lines, want) {
		t.Errorf("lines after completion = %q, want %q", reader.lines, want)
	}
	if result.Restarted {
		t.Error("completing a line should not restart the read")
	}
	if want := int64(len(`{"half":1}` + "\n")); result.Bytes != want {
		t.Errorf("Bytes = %d, want %d (the whole completed line)", result.Bytes, want)
	}
}

func TestReadAppendedLines_Pending(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantLines   []string
		wantPending string
	}{
		{name: "complete JSON without a newline is pending", content: "{\"a\":1}\n{\"b\":2}", wantLines: []string{`{"a":1}`}, wantPending: `{"b":2}`},
		{name: "half-written JSON is not pending", content: "{\"a\":1}\n{\"b\":", wantLines: []string{`{"a":1}`}},
		{name: "newline-terminated file has nothing pending", content: "{\"a\":1}\n", wantLines: []string{`{"a":1}`}},
		{name: "only line complete without a newline", content: `{"a":1}`, wantLines: nil, wantPending: `{"a":1}`},
		{name: "trailing whitespace is not pending", content: "{\"a\":1}\n  ", wantLines: []string{`{"a":1}`}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			reader := &tailReader{}
			result := reader.read(t, path)
			if !reflect.DeepEqual(reader.lines, tt.wantLines) {
				t.Errorf("lines = %q, want %q", reader.lines, tt.wantLines)
			}
			if string(result.Pending) != tt.wantPending {
				t.Errorf("Pending = %q, want %q", result.Pending, tt.wantPending)
			}
		})
	}
}

// A pending record is delivered as a line exactly once, after its newline arrives.
func TestReadAppendedLines_PendingIsNotConsumed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte("{\"a\":1}\n{\"b\":2}"), 0o644); err != nil {
		t.Fatal(err)
	}

	reader := &tailReader{}
	reader.read(t, path)
	again := reader.read(t, path)
	if string(again.Pending) != `{"b":2}` || again.Bytes != 0 || again.Restarted {
		t.Errorf("re-read without changes = %+v, want the same pending record and nothing consumed", again)
	}

	appendFile(t, path, "\n{\"c\":3}\n")
	result := reader.read(t, path)
	if want := []string{`{"a":1}`, `{"b":2}`, `{"c":3}`}; !reflect.DeepEqual(reader.lines, want) {
		t.Errorf("lines = %q, want %q", reader.lines, want)
	}
	if result.Restarted || result.Pending != nil {
		t.Errorf("result = %+v, want a continued read with nothing pending", result)
	}
}

func TestReadAppendedLines_MissingFile(t *testing.T) {
	cursor := TailCursor{}
	_, err := ReadAppendedLines(filepath.Join(t.TempDir(), "missing.jsonl"), &cursor, func() {}, func([]byte) error { return nil })
	if !os.IsNotExist(err) {
		t.Errorf("error = %v, want not-exist", err)
	}
}

func TestTailStates(t *testing.T) {
	states := NewTailStates[int](time.Minute)
	states.Put("idle", 1)
	states.Put("active", 2)

	later := time.Now().Add(2 * time.Minute)
	states.mu.Lock()
	states.entries["active"].lastUsed = later
	states.mu.Unlock()

	states.EvictIdle(later)
	if _, ok := states.Get("idle"); ok {
		t.Error("idle entry survived eviction")
	}
	if got, ok := states.Get("active"); !ok || got != 2 {
		t.Errorf("active entry = %d, %v; want 2, true", got, ok)
	}

	states.Forget("active")
	if states.Len() != 0 {
		t.Errorf("Len() = %d after Forget, want 0", states.Len())
	}
}
