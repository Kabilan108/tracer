package claudecode

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tracer-ai/tracer-cli/pkg/spi"
)

// claudeTails is the incremental parse cache behind emitClaudeSessions.
// Why package-level: emitClaudeSessions is the per-file parse entry point the
// watchers call, so the cache must outlive individual calls without threading
// state through every watcher.
var claudeTails = newClaudeTailCache(spi.DefaultTailIdleTTL)

// claudeFileTail is the incremental parse state of one session file.
type claudeFileTail struct {
	cursor spi.TailCursor
	parser *claudeFileParser
}

// claudeSessionIDEntry caches the first sessionId of a file, which decides
// which session the file's records belong to.
type claudeSessionIDEntry struct {
	dev       uint64
	inode     uint64
	size      int64
	sessionID string
}

type claudeTailCache struct {
	// mu serializes incremental parses. A session spans several files and a
	// summary line mutates the record before it, so reading, assembling and
	// converting one session must not interleave with another parse.
	mu    sync.Mutex
	files *spi.TailStates[*claudeFileTail]
	// sessionIDs is not subject to the idle TTL: an entry is a few dozen bytes,
	// and keeping it spares re-reading the head of every sibling file in a
	// project directory on each write event.
	sessionIDs map[string]claudeSessionIDEntry

	// bytesRead counts session-file bytes consumed by incremental reads, so
	// tests can assert that work follows appended bytes rather than file size.
	bytesRead atomic.Int64
}

func newClaudeTailCache(idleTTL time.Duration) *claudeTailCache {
	return &claudeTailCache{
		files:      spi.NewTailStates[*claudeFileTail](idleTTL),
		sessionIDs: make(map[string]claudeSessionIDEntry),
	}
}

// forget drops all cached state for path, e.g. after it was removed or renamed away.
func (c *claudeTailCache) forget(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files.Forget(path)
	delete(c.sessionIDs, path)
}

// agentSessionsForFile returns the session changedFile belongs to, rebuilt from
// cached per-file records after reading only bytes appended since the last call.
// The result matches a full ParseProjectSessionsForSession over the same
// newline-terminated files, because each file is parsed with the same
// claudeFileParser rules and the same assembly steps run over the same
// records in the same file order.
func (c *claudeTailCache) agentSessionsForFile(projectDir string, changedFile string, debugRaw bool) ([]*spi.AgentChatSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	targetSessionID, err := c.sessionIDLocked(changedFile)
	if err != nil {
		return nil, err
	}
	if targetSessionID == "" {
		return nil, nil
	}

	var allRecords []JSONLRecord
	walkErr := filepath.Walk(projectDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		foundSessionID, idErr := c.sessionIDLocked(path)
		if idErr != nil {
			slog.Warn("agentSessionsForFile: Failed to check session ID, parsing anyway",
				"file", path,
				"error", idErr)
		} else if foundSessionID != targetSessionID {
			return nil
		}

		records, err := c.readLocked(path)
		if err != nil {
			return fmt.Errorf("failed to parse file %s: %w", path, err)
		}
		allRecords = append(allRecords, records...)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("failed to scan project directory %s: %w", projectDir, walkErr)
	}

	parser := NewJSONLParser()
	parser.assembleSessions(allRecords)

	var result []*spi.AgentChatSession
	for _, session := range parser.Sessions {
		if len(session.Records) == 0 || session.SessionUuid != targetSessionID {
			continue
		}
		if agentSession := convertToAgentChatSession(session, "", debugRaw); agentSession != nil {
			result = append(result, agentSession)
		}
	}
	return result, nil
}

// sessionIDLocked returns the first sessionId in path, re-reading the file
// head only when the file was replaced, truncated, or had no sessionId yet and grew.
func (c *claudeTailCache) sessionIDLocked(path string) (string, error) {
	stat, err := spi.StatFile(path)
	if err != nil {
		delete(c.sessionIDs, path)
		return "", err
	}

	if entry, ok := c.sessionIDs[path]; ok &&
		entry.dev == stat.Dev && entry.inode == stat.Inode && stat.Size >= entry.size &&
		(entry.sessionID != "" || stat.Size == entry.size) {
		return entry.sessionID, nil
	}

	sessionID, err := extractSessionIDFromFile(path)
	if err != nil {
		delete(c.sessionIDs, path)
		return "", err
	}
	c.sessionIDs[path] = claudeSessionIDEntry{
		dev:       stat.Dev,
		inode:     stat.Inode,
		size:      stat.Size,
		sessionID: sessionID,
	}
	return sessionID, nil
}

// readLocked brings the cached records of path up to date and returns them.
func (c *claudeTailCache) readLocked(path string) ([]JSONLRecord, error) {
	tail, ok := c.files.Get(path)
	if !ok {
		tail = &claudeFileTail{}
	}

	result, err := spi.ReadAppendedLines(path, &tail.cursor,
		func() {
			slog.Info("Parsing session file", "file", path)
			tail.parser = newClaudeFileParser(path)
		},
		func(line []byte) error {
			return tail.parser.consumeLine(line)
		})
	c.bytesRead.Add(result.Bytes)
	if err != nil {
		c.files.Forget(path)
		return nil, err
	}

	c.files.Put(path, tail)
	return tail.parser.records, nil
}
