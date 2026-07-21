package session

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// ErrSessionNotFound identifies a session ID that matched no archived
// transcript, so callers can branch with errors.Is instead of matching
// error-message text.
var ErrSessionNotFound = errors.New("session not found")

func LockTranscript(path string) (func(), error) {
	lockPath := path + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open transcript lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("lock transcript: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}, nil
}

func ScanArchives(roots []string) ([]Metadata, error) {
	return scanArchives(roots, false)
}

// ScanArchivesStrict scans archives without suppressing root traversal errors.
func ScanArchivesStrict(roots []string) ([]Metadata, error) {
	return scanArchives(roots, true)
}

func scanArchives(roots []string, strict bool) ([]Metadata, error) {
	seenRoots := make(map[string]struct{})
	seenPaths := make(map[string]struct{})
	result := make([]Metadata, 0)
	for _, configuredRoot := range roots {
		root, err := filepath.EvalSymlinks(filepath.Clean(configuredRoot))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if strict {
				return nil, fmt.Errorf("resolve archive root %s: %w", configuredRoot, err)
			}
			slog.Warn("Skipping inaccessible archive root", "root", configuredRoot, "error", err)
			continue
		}
		root, err = filepath.Abs(root)
		if err != nil {
			if strict {
				return nil, fmt.Errorf("resolve archive root %s: %w", configuredRoot, err)
			}
			slog.Warn("Skipping unresolved archive root", "root", configuredRoot, "error", err)
			continue
		}
		if _, ok := seenRoots[root]; ok {
			continue
		}
		seenRoots[root] = struct{}{}
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if strict {
					return walkErr
				}
				slog.Warn("Skipping inaccessible archive path", "path", path, "error", walkErr)
				if entry != nil && entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(path), ".md") {
				return nil
			}
			absolutePath, err := filepath.Abs(path)
			if err != nil {
				return fmt.Errorf("resolve archive path %s: %w", path, err)
			}
			if _, ok := seenPaths[absolutePath]; ok {
				return nil
			}
			seenPaths[absolutePath] = struct{}{}
			content, err := os.ReadFile(absolutePath)
			if err != nil {
				slog.Warn("Failed to read archived transcript", "path", absolutePath, "error", err)
				return nil
			}
			metadata, _, err := ParseFrontmatter(content)
			if err != nil {
				slog.Warn("Skipping transcript without valid frontmatter", "path", absolutePath, "error", err)
				return nil
			}
			metadata.Path = absolutePath
			result = append(result, metadata)
			return nil
		})
		if err != nil {
			if strict {
				return nil, fmt.Errorf("walk archive root %s: %w", root, err)
			}
			slog.Warn("Archive root scan was incomplete", "root", root, "error", err)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Ended != result[j].Ended {
			return result[i].Ended > result[j].Ended
		}
		return result[i].Path < result[j].Path
	})
	slog.Debug("Scanned archived transcripts", "roots", roots, "sessions", len(result))
	return result, nil
}

func ResolveTranscript(roots []string, sessionIDOrPath string) (Metadata, error) {
	return resolveTranscript(roots, sessionIDOrPath, false)
}

// ResolveTranscriptStrict resolves a session ID only after every root was walked successfully.
func ResolveTranscriptStrict(roots []string, sessionIDOrPath string) (Metadata, error) {
	return resolveTranscript(roots, sessionIDOrPath, true)
}

func resolveTranscript(roots []string, sessionIDOrPath string, strict bool) (Metadata, error) {
	if strings.EqualFold(filepath.Ext(sessionIDOrPath), ".md") || strings.ContainsRune(sessionIDOrPath, filepath.Separator) {
		absolutePath, err := filepath.Abs(sessionIDOrPath)
		if err != nil {
			return Metadata{}, fmt.Errorf("resolve transcript path: %w", err)
		}
		content, err := os.ReadFile(absolutePath)
		if err != nil {
			return Metadata{}, fmt.Errorf("read transcript: %w", err)
		}
		metadata, _, err := ParseFrontmatter(content)
		if err != nil {
			return Metadata{}, err
		}
		metadata.Path = absolutePath
		return metadata, nil
	}

	var sessions []Metadata
	var err error
	if strict {
		sessions, err = ScanArchivesStrict(roots)
	} else {
		sessions, err = ScanArchives(roots)
	}
	if err != nil {
		return Metadata{}, err
	}
	matches := make([]Metadata, 0)
	for _, metadata := range sessions {
		if metadata.SessionID == sessionIDOrPath {
			matches = append(matches, metadata)
		}
	}
	if len(matches) == 0 {
		return Metadata{}, fmt.Errorf("session %q: %w", sessionIDOrPath, ErrSessionNotFound)
	}
	if len(matches) > 1 {
		paths := make([]string, 0, len(matches))
		for _, match := range matches {
			paths = append(paths, match.Path)
		}
		return Metadata{}, fmt.Errorf("session %q is ambiguous; use a transcript path:\n- %s", sessionIDOrPath, strings.Join(paths, "\n- "))
	}
	return matches[0], nil
}

func WriteMetadata(metadata Metadata) error {
	unlock, err := LockTranscript(metadata.Path)
	if err != nil {
		return err
	}
	defer unlock()

	content, err := os.ReadFile(metadata.Path)
	if err != nil {
		return fmt.Errorf("read transcript: %w", err)
	}
	current, body, err := ParseFrontmatter(content)
	if err != nil {
		return err
	}
	current = ApplyAnnotations(current, Annotations{Outcome: metadata.Outcome, Tags: metadata.Tags})
	frontmatter, err := RenderFrontmatter(current)
	if err != nil {
		return err
	}
	updated := append([]byte(frontmatter), body...)
	if string(updated) == string(content) {
		return nil
	}
	temporaryPath := metadata.Path + ".tmp"
	if err := os.WriteFile(temporaryPath, updated, 0o644); err != nil {
		return fmt.Errorf("write transcript metadata: %w", err)
	}
	if err := os.Rename(temporaryPath, metadata.Path); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("replace transcript metadata: %w", err)
	}
	slog.Info("Updated transcript metadata", "session_id", metadata.SessionID, "path", metadata.Path)
	return nil
}
