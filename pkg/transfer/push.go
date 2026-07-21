package transfer

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/tracer-ai/tracer-cli/pkg/session"
)

// frontmatterProbeBytes bounds how much of each file is retained during
// hashing to validate frontmatter. Real frontmatter is well under 1 KiB;
// 32 KiB leaves ample headroom without buffering whole transcripts.
const frontmatterProbeBytes = 32 << 10

const manifestPath = ".tracer-push-manifest.json"

// Manifest identifies the archive transfer protocol used by a tar stream.
type Manifest struct {
	Protocol   int    `json:"protocol"`
	SenderHost string `json:"sender_host"`
	Count      int    `json:"count"`
}

// PendingFile is an archived transcript that differs from the push cursor.
type PendingFile struct {
	RelPath     string
	Size        int64
	ContentHash string
}

// ScanResult summarizes a primary archive scan.
type ScanResult struct {
	Files    []PendingFile
	AllPaths []string
	Scanned  int
	Skipped  int

	// Invalid counts Markdown files without parseable frontmatter. They are
	// excluded from the push entirely: list and get already ignore them, and
	// sending them would make the receiver fail every batch forever (legacy
	// pre-frontmatter archives cannot be regenerated once provider data is
	// gone).
	Invalid int
}

// PushSummary describes one completed or failed push attempt.
type PushSummary struct {
	Remote      string
	Scanned     int
	Transferred int
	Bytes       int64
	Skipped     int
	Invalid     int
	Failed      int
	Duration    time.Duration
}

// TarResult describes the files actually written and safe to checkpoint.
type TarResult struct {
	Bytes   int64
	Sent    int
	Skipped int
	Stable  []CursorEntry
}

// SendTar delivers a generated tar stream and returns only after the receiver succeeds.
type SendTar func(writeTar func(io.Writer) (TarResult, error)) (TarResult, error)

// PushOptions configures one cursor-aware archive push.
type PushOptions struct {
	Remote      string
	ArchiveRoot string
	StateDBPath string
	SenderHost  string
	DryRun      bool
	Output      io.Writer
	Send        SendTar
}

// Push scans, sends changed files, and advances the cursor only after delivery succeeds.
func Push(options PushOptions) (summary PushSummary, err error) {
	started := time.Now()
	summary.Remote = options.Remote
	defer func() {
		summary.Duration = time.Since(started)
		LogPushSummary(summary)
	}()

	unlock, err := acquirePushLock(options.StateDBPath, options.Remote)
	if err != nil {
		summary.Failed++
		return summary, err
	}
	defer unlock()

	var cursor *CursorStore
	var hashes map[string]string
	if options.DryRun {
		hashes, err = LoadCursorHashesReadOnly(options.StateDBPath, options.Remote)
	} else {
		cursor, err = OpenCursorStore(options.StateDBPath)
		if err == nil {
			defer cursor.Close()
			hashes, err = cursor.LoadHashes(options.Remote)
		}
	}
	if err != nil {
		summary.Failed++
		return summary, err
	}
	scan, err := ScanPending(options.ArchiveRoot, hashes)
	summary.Scanned = scan.Scanned
	summary.Skipped = scan.Skipped
	summary.Invalid = scan.Invalid
	if err != nil {
		summary.Failed++
		return summary, err
	}

	if options.DryRun {
		if options.Output == nil {
			options.Output = io.Discard
		}
		for _, file := range scan.Files {
			if _, err := fmt.Fprintln(options.Output, file.RelPath); err != nil {
				summary.Failed++
				return summary, fmt.Errorf("write dry-run output: %w", err)
			}
		}
		return summary, nil
	}
	if len(scan.Files) == 0 {
		if err := cursor.CommitPush(options.Remote, nil, scan.AllPaths, time.Now().UTC()); err != nil {
			summary.Failed++
			return summary, err
		}
		return summary, nil
	}
	if options.Send == nil {
		summary.Failed++
		return summary, fmt.Errorf("push transport is required")
	}

	result, err := options.Send(func(writer io.Writer) (TarResult, error) {
		return WriteTar(writer, options.SenderHost, options.ArchiveRoot, scan.Files)
	})
	summary.Bytes = result.Bytes
	summary.Transferred = result.Sent
	summary.Skipped += result.Skipped
	if err != nil {
		summary.Failed++
		return summary, err
	}
	if err := cursor.CommitPush(options.Remote, result.Stable, scan.AllPaths, time.Now().UTC()); err != nil {
		summary.Failed++
		return summary, err
	}
	return summary, nil
}

func acquirePushLock(stateDBPath, remote string) (func(), error) {
	stateDir := filepath.Dir(stateDBPath)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create push lock directory: %w", err)
	}
	lockID := sha256.Sum256([]byte(remote))
	lockPath := filepath.Join(stateDir, fmt.Sprintf("push-%x.lock", lockID[:8]))
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open push lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("push to %s already in progress", remote)
	}
	return func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}, nil
}

// ScanPending scans Markdown files under the primary archive and compares file-byte hashes.
func ScanPending(root string, cursor map[string]string) (ScanResult, error) {
	result := ScanResult{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			return nil
		}
		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("resolve archive path %s: %w", path, err)
		}
		relPath = filepath.ToSlash(relPath)
		hash, size, head, err := hashFileWithHead(path)
		if err != nil {
			return err
		}
		if _, _, parseErr := session.ParseFrontmatter(head); parseErr != nil {
			result.Invalid++
			slog.Warn("Skipping transcript without valid frontmatter", "path", relPath, "error", parseErr)
			return nil
		}
		result.Scanned++
		result.AllPaths = append(result.AllPaths, relPath)
		if cursor[relPath] == hash {
			result.Skipped++
			return nil
		}
		result.Files = append(result.Files, PendingFile{RelPath: relPath, Size: size, ContentHash: hash})
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("scan primary archive: %w", err)
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].RelPath < result.Files[j].RelPath })
	sort.Strings(result.AllPaths)
	return result, nil
}

func hashFile(path string) (string, int64, error) {
	hash, size, _, err := hashFileWithHead(path)
	return hash, size, err
}

// hashFileWithHead hashes the whole file in one pass while retaining its
// first frontmatterProbeBytes for validation, avoiding a second read.
func hashFileWithHead(path string) (string, int64, []byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, nil, fmt.Errorf("open archive file %s: %w", path, err)
	}
	defer file.Close()
	hasher := sha256.New()
	head := &bytes.Buffer{}
	reader := io.TeeReader(io.LimitReader(file, frontmatterProbeBytes), head)
	if _, err := io.Copy(hasher, reader); err != nil {
		return "", 0, nil, fmt.Errorf("hash archive file %s: %w", path, err)
	}
	rest, err := io.Copy(hasher, file)
	if err != nil {
		return "", 0, nil, fmt.Errorf("hash archive file %s: %w", path, err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), int64(head.Len()) + rest, head.Bytes(), nil
}

// WriteTar writes a protocol manifest followed by files unchanged since the scan.
func WriteTar(writer io.Writer, senderHost, archiveRoot string, files []PendingFile) (TarResult, error) {
	result := TarResult{}
	ready := make([]PendingFile, 0, len(files))
	for _, file := range files {
		hash, size, err := hashFile(filepath.Join(archiveRoot, filepath.FromSlash(file.RelPath)))
		if err != nil {
			return result, err
		}
		if hash != file.ContentHash || size != file.Size {
			result.Skipped++
			continue
		}
		ready = append(ready, file)
	}

	tarWriter := tar.NewWriter(writer)
	manifest, err := json.Marshal(Manifest{Protocol: 1, SenderHost: senderHost, Count: len(ready)})
	if err != nil {
		return result, fmt.Errorf("marshal push manifest: %w", err)
	}
	if err := writeTarEntry(tarWriter, manifestPath, manifest); err != nil {
		_ = tarWriter.Close()
		return result, err
	}

	for _, file := range ready {
		path := filepath.Join(archiveRoot, filepath.FromSlash(file.RelPath))
		input, err := os.Open(path)
		if err != nil {
			_ = tarWriter.Close()
			return result, fmt.Errorf("open archive file %s: %w", path, err)
		}
		header := &tar.Header{Name: file.RelPath, Mode: 0o644, Size: file.Size, Typeflag: tar.TypeReg}
		if err := tarWriter.WriteHeader(header); err != nil {
			_ = input.Close()
			_ = tarWriter.Close()
			return result, fmt.Errorf("write tar header %s: %w", file.RelPath, err)
		}
		hasher := sha256.New()
		written, copyErr := io.CopyN(tarWriter, io.TeeReader(input, hasher), file.Size)
		closeErr := input.Close()
		result.Bytes += written
		if copyErr != nil {
			_ = tarWriter.Close()
			return result, fmt.Errorf("write tar content %s: %w", file.RelPath, copyErr)
		}
		if closeErr != nil {
			_ = tarWriter.Close()
			return result, fmt.Errorf("close archive file %s: %w", path, closeErr)
		}
		result.Sent++
		currentHash, currentSize, err := hashFile(path)
		if err != nil {
			_ = tarWriter.Close()
			return result, err
		}
		if hex.EncodeToString(hasher.Sum(nil)) == file.ContentHash &&
			currentHash == file.ContentHash &&
			currentSize == file.Size {
			result.Stable = append(result.Stable, CursorEntry{RelPath: file.RelPath, ContentHash: file.ContentHash})
		}
	}
	if err := tarWriter.Close(); err != nil {
		return result, fmt.Errorf("close push tar: %w", err)
	}
	return result, nil
}

func writeTarEntry(writer *tar.Writer, name string, content []byte) error {
	header := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
	if err := writer.WriteHeader(header); err != nil {
		return fmt.Errorf("write tar header %s: %w", name, err)
	}
	if _, err := writer.Write(content); err != nil {
		return fmt.Errorf("write tar content %s: %w", name, err)
	}
	return nil
}

// LogPushSummary emits one wide event for a push attempt.
func LogPushSummary(summary PushSummary) {
	slog.Info("Archive push complete",
		"remote", summary.Remote,
		"scanned", summary.Scanned,
		"transferred", summary.Transferred,
		"bytes", summary.Bytes,
		"skipped", summary.Skipped,
		"invalid", summary.Invalid,
		"failed", summary.Failed,
		"duration", summary.Duration,
	)
}
