package spi

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// stubLookupUser replaces the user database with fixed entries for the
// duration of a test, so "~user" cases do not depend on the host's accounts.
func stubLookupUser(t *testing.T, homes map[string]string) {
	t.Helper()
	original := lookupUser
	lookupUser = func(name string) (*user.User, error) {
		home, ok := homes[name]
		if !ok {
			return nil, user.UnknownUserError(name)
		}
		return &user.User{Username: name, HomeDir: home}, nil
	}
	t.Cleanup(func() { lookupUser = original })
}

func TestExpandTilde(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		home    string
		want    string
		wantErr string
	}{
		{name: "empty input", path: "", want: ""},
		{name: "absolute path", path: "/srv/archive", want: "/srv/archive"},
		{name: "relative path", path: "archive/x", want: "archive/x"},
		{name: "tilde in the middle of a relative path", path: "a/~/b", want: "a/~/b"},
		{name: "tilde-user in the middle of an absolute path", path: "/x/~bob/y", want: "/x/~bob/y"},
		{name: "bare tilde", path: "~", want: "/home/me"},
		{name: "tilde slash", path: "~/", want: "/home/me"},
		{name: "tilde path", path: "~/x/y", want: "/home/me/x/y"},
		{name: "tilde path is cleaned", path: "~//x/../y", want: "/home/me/y"},
		{name: "tilde user", path: "~bob", want: "/srv/bob"},
		{name: "tilde user slash", path: "~bob/", want: "/srv/bob"},
		{name: "tilde user path", path: "~bob/x", want: "/srv/bob/x"},
		{name: "unknown user", path: "~nosuchuser/x", wantErr: "unknown user nosuchuser"},
		{name: "user without home directory", path: "~nohome/x", wantErr: "has no home directory"},
		{name: "plus is not a username", path: "~+/x", wantErr: "unknown user +"},
		{name: "unset HOME", path: "~/x", home: "-", wantErr: "$HOME is not defined"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := "/home/me"
			if tt.home == "-" {
				home = ""
			}
			t.Setenv("HOME", home)
			stubLookupUser(t, map[string]string{"bob": "/srv/bob", "nohome": ""})

			got, err := ExpandTilde(tt.path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ExpandTilde(%q) error = %v, want error containing %q", tt.path, err, tt.wantErr)
				}
				if got != "" {
					t.Errorf("ExpandTilde(%q) = %q on error, want empty", tt.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExpandTilde(%q) error = %v", tt.path, err)
			}
			if got != tt.want {
				t.Errorf("ExpandTilde(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestExpandTilde_RealUserDatabase(t *testing.T) {
	// Why: the other cases stub the lookup, so this one proves an unknown
	// name fails through os/user rather than falling back to $HOME.
	t.Setenv("HOME", "/home/me")
	got, err := ExpandTilde("~tracer-no-such-user-12/x")
	if err == nil {
		t.Fatalf("ExpandTilde() = %q, want an unknown-user error", got)
	}
}

func TestExpandCommandPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "bare command", path: "claude", want: "claude"},
		{name: "tilde path", path: "~/bin/claude", want: "/home/me/bin/claude"},
		{name: "tilde user path", path: "~bob/bin/codex", want: "/srv/bob/bin/codex"},
		{name: "unknown user stays literal", path: "~nosuchuser/bin/codex", want: "~nosuchuser/bin/codex"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", "/home/me")
			stubLookupUser(t, map[string]string{"bob": "/srv/bob"})

			if got := ExpandCommandPath(tt.path); got != tt.want {
				t.Errorf("ExpandCommandPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestGetCanonicalPath(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(t *testing.T) string
		wantErr  bool
		validate func(t *testing.T, input, result string)
	}{
		{
			name: "absolute path with correct case",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				return dir
			},
			wantErr: false,
			validate: func(t *testing.T, input, result string) {
				// After symlink resolution, the path might be different
				// (e.g., on macOS /var -> /private/var)
				// We should verify that the result is a valid canonical form
				// by checking that it resolves to the same location as the input
				inputResolved, _ := filepath.EvalSymlinks(input)
				if inputResolved == "" {
					inputResolved = input
				}
				// Both result and inputResolved should point to the same location
				// Compare them after cleaning
				if filepath.Clean(result) != filepath.Clean(inputResolved) {
					t.Errorf("GetCanonicalPath(%q) = %q, want %q (after symlink resolution)", input, result, inputResolved)
				}
			},
		},
		{
			name: "relative path converts to absolute",
			setup: func(t *testing.T) string {
				return "."
			},
			wantErr: false,
			validate: func(t *testing.T, input, result string) {
				if !filepath.IsAbs(result) {
					t.Errorf("GetCanonicalPath(%q) = %q, want absolute path", input, result)
				}
			},
		},
		{
			name: "non-existent path appends remaining components",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				return filepath.Join(dir, "nonexistent", "path", "components")
			},
			wantErr: false,
			validate: func(t *testing.T, input, result string) {
				if !strings.HasSuffix(result, "nonexistent/path/components") {
					t.Errorf("GetCanonicalPath(%q) = %q, want path ending with nonexistent/path/components", input, result)
				}
			},
		},
		{
			name: "deeply nested directory",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				nested := filepath.Join(dir, "level1", "level2", "level3")
				if err := os.MkdirAll(nested, 0755); err != nil {
					t.Fatal(err)
				}
				return nested
			},
			wantErr: false,
			validate: func(t *testing.T, input, result string) {
				if !strings.HasSuffix(result, "level1/level2/level3") {
					t.Errorf("GetCanonicalPath(%q) = %q, want path ending with level1/level2/level3", input, result)
				}
			},
		},
		{
			name: "path with special characters",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				special := filepath.Join(dir, "test with spaces", "special@chars!")
				if err := os.MkdirAll(special, 0755); err != nil {
					t.Fatal(err)
				}
				return special
			},
			wantErr: false,
			validate: func(t *testing.T, input, result string) {
				if !strings.Contains(result, "test with spaces") || !strings.Contains(result, "special@chars!") {
					t.Errorf("GetCanonicalPath(%q) = %q, want path containing special characters", input, result)
				}
			},
		},
		{
			name: "path with unicode characters",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				unicode := filepath.Join(dir, "测试目录", "テスト", "🚀")
				if err := os.MkdirAll(unicode, 0755); err != nil {
					t.Fatal(err)
				}
				return unicode
			},
			wantErr: false,
			validate: func(t *testing.T, input, result string) {
				if !strings.Contains(result, "测试目录") || !strings.Contains(result, "テスト") || !strings.Contains(result, "🚀") {
					t.Errorf("GetCanonicalPath(%q) = %q, want path containing unicode characters", input, result)
				}
			},
		},
		{
			name: "case-insensitive matching on macOS",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				// Create directory with specific case
				testDir := filepath.Join(dir, "TestDirectory")
				if err := os.Mkdir(testDir, 0755); err != nil {
					t.Fatal(err)
				}
				// Return path with different case
				return filepath.Join(dir, "testdirectory")
			},
			wantErr: false,
			validate: func(t *testing.T, input, result string) {
				// On case-insensitive filesystems (macOS), should return the actual case
				// On case-sensitive filesystems (Linux), it depends on what exists
				if !strings.HasSuffix(result, "TestDirectory") && !strings.HasSuffix(result, "testdirectory") {
					t.Errorf("GetCanonicalPath(%q) = %q, want path ending with TestDirectory or testdirectory", input, result)
				}
			},
		},
		{
			name: "path with trailing slashes",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				return dir + "///"
			},
			wantErr: false,
			validate: func(t *testing.T, input, result string) {
				if strings.HasSuffix(result, "/") {
					t.Errorf("GetCanonicalPath(%q) = %q, want path without trailing slashes", input, result)
				}
			},
		},
		{
			name: "partially existing path",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				existing := filepath.Join(dir, "existing")
				if err := os.Mkdir(existing, 0755); err != nil {
					t.Fatal(err)
				}
				// Return path where first part exists but second doesn't
				return filepath.Join(existing, "nonexistent", "deeper")
			},
			wantErr: false,
			validate: func(t *testing.T, input, result string) {
				if !strings.Contains(result, "existing") || !strings.Contains(result, "nonexistent") {
					t.Errorf("GetCanonicalPath(%q) = %q, want path containing both existing and nonexistent parts", input, result)
				}
			},
		},
		{
			name: "symlink resolution",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				// Create a real directory
				realDir := filepath.Join(dir, "real", "directory")
				if err := os.MkdirAll(realDir, 0755); err != nil {
					t.Fatal(err)
				}
				// Create a symlink pointing to the real directory
				symlinkPath := filepath.Join(dir, "symlink")
				if err := os.Symlink(realDir, symlinkPath); err != nil {
					t.Fatal(err)
				}
				// Return the symlink path
				return symlinkPath
			},
			wantErr: false,
			validate: func(t *testing.T, input, result string) {
				// The result should be the real path, not the symlink path
				// It should contain "real/directory" and NOT end with "symlink"
				if strings.HasSuffix(result, "symlink") {
					t.Errorf("GetCanonicalPath(%q) = %q, symlink was not resolved (result still ends with 'symlink')", input, result)
				}
				if !strings.Contains(result, "real") || !strings.Contains(result, "directory") {
					t.Errorf("GetCanonicalPath(%q) = %q, want path containing 'real' and 'directory'", input, result)
				}
			},
		},
		{
			name: "nested symlinks resolution",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				// Create a real directory
				realDir := filepath.Join(dir, "actual", "target")
				if err := os.MkdirAll(realDir, 0755); err != nil {
					t.Fatal(err)
				}
				// Create first level symlink
				symlink1 := filepath.Join(dir, "link1")
				if err := os.Symlink(realDir, symlink1); err != nil {
					t.Fatal(err)
				}
				// Create second level symlink pointing to first symlink
				symlink2 := filepath.Join(dir, "link2")
				if err := os.Symlink(symlink1, symlink2); err != nil {
					t.Fatal(err)
				}
				// Return the nested symlink path
				return symlink2
			},
			wantErr: false,
			validate: func(t *testing.T, input, result string) {
				// The result should be the real path, resolving all symlinks
				if strings.Contains(result, "link1") || strings.Contains(result, "link2") {
					t.Errorf("GetCanonicalPath(%q) = %q, nested symlinks were not fully resolved", input, result)
				}
				if !strings.Contains(result, "actual") || !strings.Contains(result, "target") {
					t.Errorf("GetCanonicalPath(%q) = %q, want path containing 'actual' and 'target'", input, result)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := tt.setup(t)

			result, err := GetCanonicalPath(input)

			if tt.wantErr {
				if err == nil {
					t.Errorf("GetCanonicalPath() error = nil, wantErr %v", tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Errorf("GetCanonicalPath() unexpected error = %v", err)
				return
			}

			// Result should always be absolute
			if !filepath.IsAbs(result) {
				t.Errorf("GetCanonicalPath(%q) = %q, want absolute path", input, result)
			}

			// Run custom validation if provided
			if tt.validate != nil {
				tt.validate(t, input, result)
			}
		})
	}
}

// TestSetDebugBaseDir tests the debug base dir override mechanism
func TestSetDebugBaseDir(t *testing.T) {
	// Clean up after test to avoid affecting other tests
	defer SetDebugBaseDir("")

	t.Run("default path without override", func(t *testing.T) {
		SetDebugBaseDir("")
		result := GetDebugDir("test-session")
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("UserHomeDir() error = %v", err)
		}
		expected := filepath.Join(home, ".local", "state", "tracer", "debug", "test-session")
		if result != expected {
			t.Errorf("GetDebugDir() = %q, want %q", result, expected)
		}
	})

	t.Run("override changes output path", func(t *testing.T) {
		SetDebugBaseDir("/custom/debug")
		result := GetDebugDir("test-session")
		expected := filepath.Join("/custom/debug", "test-session")
		if result != expected {
			t.Errorf("GetDebugDir() = %q, want %q", result, expected)
		}
	})

	t.Run("empty override restores default", func(t *testing.T) {
		SetDebugBaseDir("/custom/debug")
		SetDebugBaseDir("")
		result := GetDebugDir("test-session")
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("UserHomeDir() error = %v", err)
		}
		expected := filepath.Join(home, ".local", "state", "tracer", "debug", "test-session")
		if result != expected {
			t.Errorf("GetDebugDir() = %q, want %q", result, expected)
		}
	})
}

// TestGenerateReadableName tests the generation of human-readable session names
func TestGenerateReadableName(t *testing.T) {
	tests := []struct {
		name     string
		message  string
		expected string
	}{
		{
			name:     "Empty message returns empty",
			message:  "",
			expected: "",
		},
		{
			name:     "Short message returned as-is",
			message:  "Let's create a session!",
			expected: "Let's create a session!",
		},
		{
			name:     "Message with newlines normalized",
			message:  "First line\nSecond line\nThird line",
			expected: "First line Second line Third line",
		},
		{
			name:     "Message with multiple spaces normalized",
			message:  "Hello    world     how   are   you",
			expected: "Hello world how are you",
		},
		{
			name:     "Long message truncated at word boundary",
			message:  "This is a very long message that exceeds one hundred characters and should be truncated at a word boundary to avoid breaking words in the middle",
			expected: "This is a very long message that exceeds one hundred characters and should be truncated at a word...",
		},
		{
			name:     "Long message without spaces truncated at exactly 100 chars",
			message:  strings.Repeat("a", 150),
			expected: strings.Repeat("a", 100) + "...",
		},
		{
			name:     "Exactly 100 characters not truncated",
			message:  strings.Repeat("a", 100),
			expected: strings.Repeat("a", 100),
		},
		{
			name:     "Message with tabs and mixed whitespace",
			message:  "Hello\tworld\n\nHow\t\tare   you?",
			expected: "Hello world How are you?",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GenerateReadableName(tt.message)
			if result != tt.expected {
				t.Errorf("GenerateReadableName() = %q, want %q", result, tt.expected)
			}
		})
	}
}
