package transfer

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tracer-ai/tracer-cli/pkg/session"
)

// maxTranscriptBytes bounds one incoming transcript. Real transcripts are
// text and rarely exceed a few hundred KiB; 64 MiB leaves generous headroom
// while keeping a hostile tar header from ballooning receiver memory.
const maxTranscriptBytes = 64 << 20

// ReceiveSummary describes the files applied from one tar stream.
type ReceiveSummary struct {
	Received int
	Merged   int
	Created  int
	Failed   int
	Duration time.Duration
}

// Receive consumes one push tar stream and atomically updates the destination archive.
func Receive(reader io.Reader, dest string) (summary ReceiveSummary, err error) {
	started := time.Now()
	defer func() {
		summary.Duration = time.Since(started)
		slog.Info("Archive receive complete",
			"received", summary.Received,
			"merged", summary.Merged,
			"created", summary.Created,
			"failed", summary.Failed,
			"duration", summary.Duration,
		)
	}()

	if err := os.MkdirAll(dest, 0o755); err != nil {
		summary.Failed++
		return summary, fmt.Errorf("create destination root: %w", err)
	}
	resolvedDest, err := filepath.EvalSymlinks(dest)
	if err != nil {
		summary.Failed++
		return summary, fmt.Errorf("resolve destination root: %w", err)
	}
	resolvedDest, err = filepath.Abs(resolvedDest)
	if err != nil {
		summary.Failed++
		return summary, fmt.Errorf("resolve absolute destination root: %w", err)
	}

	tarReader := tar.NewReader(reader)
	header, err := tarReader.Next()
	if err != nil {
		summary.Failed++
		if errors.Is(err, io.EOF) {
			return summary, fmt.Errorf("push manifest is missing")
		}
		return summary, fmt.Errorf("read push manifest: %w", err)
	}
	if header.Name != manifestPath || header.Typeflag != tar.TypeReg {
		summary.Failed++
		return summary, fmt.Errorf("first tar entry must be %s", manifestPath)
	}
	var manifest Manifest
	if err := json.NewDecoder(tarReader).Decode(&manifest); err != nil {
		summary.Failed++
		return summary, fmt.Errorf("decode push manifest: %w", err)
	}
	if manifest.Protocol != 1 {
		summary.Failed++
		return summary, fmt.Errorf("unsupported push protocol %d; upgrade tracer on the receiver", manifest.Protocol)
	}

	entryCount := 0
	fileErrors := make([]error, 0)
	for {
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			summary.Failed++
			return summary, fmt.Errorf("read push tar: %w", nextErr)
		}
		entryCount++
		if err := validateRelativePath(header.Name); err != nil {
			summary.Failed++
			fileErrors = append(fileErrors, err)
			slog.Warn("Skipping unsafe incoming archive path", "path", header.Name, "error", err)
			continue
		}
		if header.Typeflag != tar.TypeReg {
			summary.Failed++
			fileErrors = append(fileErrors, fmt.Errorf("unsupported tar entry %q: type %d is not a regular file", header.Name, header.Typeflag))
			slog.Warn("Skipping unsupported tar entry", "path", header.Name, "type", header.Typeflag)
			continue
		}
		// LimitReader caps memory per entry: tar headers state their own
		// size, so a lying or hostile sender could otherwise OOM the
		// receiver before frontmatter validation ever runs.
		incoming, readErr := io.ReadAll(io.LimitReader(tarReader, maxTranscriptBytes+1))
		if readErr != nil {
			summary.Failed++
			return summary, fmt.Errorf("read incoming file %s: %w", header.Name, readErr)
		}
		if len(incoming) > maxTranscriptBytes {
			summary.Failed++
			fileErrors = append(fileErrors, fmt.Errorf("incoming file %s exceeds %d byte transcript limit", header.Name, maxTranscriptBytes))
			slog.Warn("Skipping oversized incoming transcript", "path", header.Name, "limit_bytes", maxTranscriptBytes)
			continue
		}
		if _, _, parseErr := session.ParseFrontmatter(incoming); parseErr != nil {
			summary.Failed++
			fileErrors = append(fileErrors, fmt.Errorf("parse incoming frontmatter %s: %w", header.Name, parseErr))
			slog.Warn("Skipping incoming transcript with invalid frontmatter", "path", header.Name, "error", parseErr)
			continue
		}
		target := filepath.Join(resolvedDest, filepath.FromSlash(header.Name))
		merged, writeErr := writeReceivedFile(resolvedDest, target, incoming)
		if writeErr != nil {
			summary.Failed++
			fileErrors = append(fileErrors, fmt.Errorf("receive %s: %w", header.Name, writeErr))
			slog.Warn("Failed to write incoming transcript", "path", header.Name, "error", writeErr)
			continue
		}
		summary.Received++
		if merged {
			summary.Merged++
		} else {
			summary.Created++
		}
	}

	if entryCount != manifest.Count {
		summary.Failed++
		fileErrors = append(fileErrors, fmt.Errorf("push manifest declared %d files but contained %d", manifest.Count, entryCount))
		slog.Warn("Push manifest count mismatch", "declared", manifest.Count, "entries", entryCount)
	}
	if summary.Failed > 0 {
		return summary, fmt.Errorf("receive completed with %d failed file(s): %w", summary.Failed, errors.Join(fileErrors...))
	}
	return summary, nil
}

func validateRelativePath(path string) error {
	if path == "" || filepath.IsAbs(path) || strings.HasPrefix(path, "/") {
		return fmt.Errorf("unsafe archive path %q: absolute paths are not allowed", path)
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return fmt.Errorf("unsafe archive path %q: path traversal is not allowed", path)
		}
	}
	return nil
}

func writeReceivedFile(resolvedDest, target string, incoming []byte) (bool, error) {
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return false, fmt.Errorf("create transcript directory: %w", err)
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return false, fmt.Errorf("resolve transcript directory: %w", err)
	}
	resolvedParent, err = filepath.Abs(resolvedParent)
	if err != nil {
		return false, fmt.Errorf("resolve absolute transcript directory: %w", err)
	}
	rootPrefix := resolvedDest
	if !strings.HasSuffix(rootPrefix, string(filepath.Separator)) {
		rootPrefix += string(filepath.Separator)
	}
	if resolvedParent != resolvedDest && !strings.HasPrefix(resolvedParent, rootPrefix) {
		return false, fmt.Errorf("resolved transcript directory %s escapes destination %s", resolvedParent, resolvedDest)
	}
	unlock, err := session.LockTranscript(target)
	if err != nil {
		return false, err
	}
	defer unlock()

	merged := false
	content := incoming
	existing, readErr := os.ReadFile(target)
	if readErr == nil {
		content, err = mergeTranscriptAnnotations(existing, incoming)
		if err != nil {
			slog.Warn("Replacing existing transcript with invalid frontmatter", "path", target, "error", err)
			content = incoming
		}
		merged = true
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return false, fmt.Errorf("read existing transcript: %w", readErr)
	}

	temporaryPath := target + ".tmp"
	if err := os.WriteFile(temporaryPath, content, 0o644); err != nil {
		return false, fmt.Errorf("write temporary transcript: %w", err)
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		_ = os.Remove(temporaryPath)
		return false, fmt.Errorf("replace transcript: %w", err)
	}
	return merged, nil
}

func mergeTranscriptAnnotations(existing, incoming []byte) ([]byte, error) {
	existingMetadata, _, err := session.ParseFrontmatter(existing)
	if err != nil {
		return nil, fmt.Errorf("parse existing frontmatter: %w", err)
	}
	incomingMetadata, body, err := session.ParseFrontmatter(incoming)
	if err != nil {
		return nil, fmt.Errorf("parse incoming frontmatter: %w", err)
	}

	annotations := session.Annotations{
		Outcome: incomingMetadata.Outcome,
		Tags:    append(append([]string(nil), existingMetadata.Tags...), incomingMetadata.Tags...),
	}
	if existingMetadata.Outcome != "" {
		annotations.Outcome = existingMetadata.Outcome
	}
	incomingMetadata = session.ApplyAnnotations(incomingMetadata, annotations)
	frontmatter, err := session.RenderFrontmatter(incomingMetadata)
	if err != nil {
		return nil, err
	}
	return append([]byte(frontmatter), body...), nil
}
