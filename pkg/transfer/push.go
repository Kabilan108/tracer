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
	"slices"
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

// NoPushTag is the reserved tag that keeps a transcript on the host it ran on.
// It lives in the transcript's own frontmatter, which sync carries forward on
// every re-render, so a live session that keeps growing stays excluded.
const NoPushTag = "no-push"

const (
	protocolBase = 1
	// protocolRetract is sent only when a stream carries retractions. Pushes
	// without retractions stay on protocolBase so receivers running an older
	// release keep working; a retraction sent to one of those receivers is
	// rejected loudly instead of being silently ignored, which leaves the
	// sender cursor untouched so the retraction is retried after the upgrade.
	protocolRetract = 2
)

// Manifest identifies the archive transfer protocol used by a tar stream.
type Manifest struct {
	Protocol   int    `json:"protocol"`
	SenderHost string `json:"sender_host"`
	Count      int    `json:"count"`

	// Retract lists archive paths the receiver must delete because the sender
	// tagged them no-push after they had already been pushed.
	Retract []string `json:"retract,omitempty"`
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

	// Excluded lists no-push transcripts this remote has never received.
	// Retract lists no-push transcripts the cursor shows this remote already
	// holds. Both stay out of AllPaths so a successful push prunes their
	// cursor rows; removing the tag later then re-sends the file in full.
	Excluded []string
	Retract  []string
}

// PushSummary describes one completed or failed push attempt.
type PushSummary struct {
	Remote      string
	Scanned     int
	Transferred int
	Bytes       int64
	Skipped     int
	Invalid     int
	// Excluded counts every no-push transcript; Retracted is the subset the
	// remote held and has now deleted.
	Excluded  int
	Retracted int
	Failed    int
	Duration  time.Duration
}

// TarResult describes the files actually written and the hashes the receiver
// holds once the stream is accepted.
type TarResult struct {
	Bytes     int64
	Sent      int
	Skipped   int
	Delivered []CursorEntry
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
	summary.Excluded = len(scan.Excluded) + len(scan.Retract)
	if err != nil {
		summary.Failed++
		return summary, err
	}

	if options.DryRun {
		if err := writeDryRun(options.Output, scan); err != nil {
			summary.Failed++
			return summary, err
		}
		return summary, nil
	}
	if len(scan.Files) == 0 && len(scan.Retract) == 0 {
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
		return WriteTar(writer, options.SenderHost, options.ArchiveRoot, scan.Files, scan.Retract)
	})
	summary.Bytes = result.Bytes
	summary.Transferred = result.Sent
	summary.Skipped += result.Skipped
	if err != nil {
		summary.Failed++
		return summary, err
	}
	// The receiver fails the whole stream when any retraction fails, so a
	// successful send means every listed path is gone from the remote.
	summary.Retracted = len(scan.Retract)
	if err := cursor.CommitPush(options.Remote, result.Delivered, scan.AllPaths, time.Now().UTC()); err != nil {
		summary.Failed++
		return summary, err
	}
	return summary, nil
}

// writeDryRun prints pending paths bare, as before, so existing consumers of
// the dry-run output keep working; no-push paths carry a prefix saying what a
// real push would do with them.
func writeDryRun(output io.Writer, scan ScanResult) error {
	if output == nil {
		output = io.Discard
	}
	lines := make([]string, 0, len(scan.Files)+len(scan.Excluded)+len(scan.Retract))
	for _, file := range scan.Files {
		lines = append(lines, file.RelPath)
	}
	for _, relPath := range scan.Excluded {
		lines = append(lines, "excluded: "+relPath)
	}
	for _, relPath := range scan.Retract {
		lines = append(lines, "retract: "+relPath)
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(output, line); err != nil {
			return fmt.Errorf("write dry-run output: %w", err)
		}
	}
	return nil
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
		// Excluding invalid files from AllPaths means a previously-pushed
		// transcript whose frontmatter is later corrupted on disk has its
		// cursor row pruned; once repaired it is re-sent in full. That is
		// intentional: the receiver merge is idempotent, and a repaired file
		// should reach the remote rather than be skipped by a stale hash.
		metadata, _, parseErr := session.ParseFrontmatter(head)
		if parseErr != nil {
			result.Invalid++
			slog.Warn("Skipping transcript without valid frontmatter", "path", relPath, "error", parseErr)
			return nil
		}
		if hasNoPushTag(metadata.Tags) {
			// A cursor row means this remote received an earlier version
			// before the tag was added, so it must be told to delete it.
			if _, pushed := cursor[relPath]; pushed {
				result.Retract = append(result.Retract, relPath)
			} else {
				result.Excluded = append(result.Excluded, relPath)
			}
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
	sort.Strings(result.Excluded)
	sort.Strings(result.Retract)
	return result, nil
}

// hasNoPushTag compares case-insensitively because hand-edited frontmatter is
// not normalized until Tracer next rewrites it, and a missed match would push
// a transcript the user meant to keep local.
func hasNoPushTag(tags []string) bool {
	return slices.ContainsFunc(tags, func(tag string) bool {
		return strings.EqualFold(strings.TrimSpace(tag), NoPushTag)
	})
}

// hashFile is the head-free path used by WriteTar, where the probe buffer of
// hashFileWithHead would be allocated twice per file only to be discarded.
func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("open archive file %s: %w", path, err)
	}
	defer file.Close()
	hasher := sha256.New()
	size, err := io.Copy(hasher, file)
	if err != nil {
		return "", 0, fmt.Errorf("hash archive file %s: %w", path, err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), size, nil
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

// WriteTar writes a protocol manifest, carrying any retractions, followed by
// files unchanged since the scan.
func WriteTar(writer io.Writer, senderHost, archiveRoot string, files []PendingFile, retract []string) (TarResult, error) {
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
	protocol := protocolBase
	if len(retract) > 0 {
		protocol = protocolRetract
	}
	manifest, err := json.Marshal(Manifest{Protocol: protocol, SenderHost: senderHost, Count: len(ready), Retract: retract})
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
		// Checkpoint the bytes the receiver actually got, not the scan hash.
		// If the file changed mid-stream, the local hash no longer matches
		// and the next push re-sends it. If the change was a no-push tag, the
		// cursor row is what lets the next push retract the copy the remote
		// already received.
		result.Delivered = append(result.Delivered, CursorEntry{
			RelPath:     file.RelPath,
			ContentHash: hex.EncodeToString(hasher.Sum(nil)),
		})
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
		"excluded", summary.Excluded,
		"retracted", summary.Retracted,
		"failed", summary.Failed,
		"duration", summary.Duration,
	)
}
