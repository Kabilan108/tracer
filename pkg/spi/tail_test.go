package spi

import (
	"os"
	"path/filepath"
	"reflect"
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
