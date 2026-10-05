package codexcli

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tracer-ai/tracer-cli/pkg/spi"
)

// codexTails is the incremental read cache behind processCodexSessionFile.
// Why package-level: processCodexSessionFile is the per-file parse entry point
// the watcher calls, so the cache must outlive individual calls without
// threading state through every watcher.
var codexTails = newCodexTailCache(spi.DefaultTailIdleTTL)

// codexFileTail is the incremental read state of one rollout file.
type codexFileTail struct {
	// mu makes concurrent parses of the same file take turns; different files
	// proceed independently.
	mu     sync.Mutex
	cursor spi.TailCursor
	meta   *codexSessionMeta
	reader *codexRecordReader
}

type codexTailCache struct {
	// mu guards creating a file's tail so concurrent callers share one.
	mu    sync.Mutex
	files *spi.TailStates[*codexFileTail]

	// bytesRead counts rollout bytes consumed by incremental reads, so tests
	// can assert that work follows appended bytes rather than file size.
	bytesRead atomic.Int64
}

func newCodexTailCache(idleTTL time.Duration) *codexTailCache {
	return &codexTailCache{files: spi.NewTailStates[*codexFileTail](idleTTL)}
}

// forget drops cached state for sessionPath, e.g. after it was removed or renamed away.
func (c *codexTailCache) forget(sessionPath string) {
	c.files.Forget(sessionPath)
}

func (c *codexTailCache) tailFor(sessionPath string) *codexFileTail {
	c.mu.Lock()
	defer c.mu.Unlock()

	tail, ok := c.files.Get(sessionPath)
	if !ok {
		tail = &codexFileTail{}
		c.files.Put(sessionPath, tail)
	}
	return tail
}

// errCodexSessionRejected stops a read whose session meta the caller does not want.
var errCodexSessionRejected = errors.New("codex session rejected by caller")

// read returns the session meta and every record of sessionPath, decoding only
// the complete lines appended since the previous call. It returns a nil meta
// when the file has no complete record yet or accept rejects the session; such
// files are not cached.
// A final record without its newline yet is included, but not cached, so the
// result matches a full read without that record being counted twice.
// The returned slice is capped at its length so later appends never show
// through to a caller still using it.
func (c *codexTailCache) read(sessionPath string, accept func(*codexSessionMeta) bool) (*codexSessionMeta, []map[string]interface{}, error) {
	tail := c.tailFor(sessionPath)
	tail.mu.Lock()
	defer tail.mu.Unlock()

	result, err := spi.ReadAppendedLines(sessionPath, &tail.cursor,
		func() {
			tail.meta = nil
			tail.reader = &codexRecordReader{sessionPath: sessionPath}
		},
		func(line []byte) error {
			if tail.meta == nil {
				// The first line is the session meta. Checking it before decoding
				// the rest keeps files of other projects from being decoded and cached.
				meta, err := parseCodexSessionMeta(line)
				if err != nil {
					return err
				}
				if !accept(meta) {
					return errCodexSessionRejected
				}
				tail.meta = meta
			}
			return tail.reader.consumeLine(line)
		})
	c.bytesRead.Add(result.Bytes)

	meta := tail.meta
	switch {
	case errors.Is(err, errCodexSessionRejected):
		c.files.Forget(sessionPath)
		return nil, nil, nil
	case err != nil:
		c.files.Forget(sessionPath)
		return nil, nil, err
	case meta == nil:
		// Nothing cached yet; the file may hold just its meta line without a newline.
		c.files.Forget(sessionPath)
		if result.Pending == nil {
			return nil, nil, nil
		}
		meta, err = parseCodexSessionMeta(result.Pending)
		if err != nil {
			return nil, nil, err
		}
	}
	if !accept(meta) {
		// Cached by a caller watching a different project
		return nil, nil, nil
	}

	records := tail.reader.records
	records = records[:len(records):len(records)]
	if result.Pending != nil {
		// Decode the pending record on a copy: the capped slice makes append
		// allocate, so the cached reader never sees it.
		provisional := *tail.reader
		provisional.records = records
		if err := provisional.consumeLine(result.Pending); err != nil {
			return nil, nil, err
		}
		records = provisional.records
	}
	return meta, records, nil
}
