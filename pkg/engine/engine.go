package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	sessionpkg "github.com/tracer-ai/tracer-cli/pkg/session"
	"github.com/tracer-ai/tracer-cli/pkg/spi"
	"github.com/tracer-ai/tracer-cli/pkg/utils"
)

const (
	defaultDebounce = 750 * time.Millisecond
)

// ProcessOutcome describes how a session processing attempt changed local state.
type ProcessOutcome string

const (
	OutcomeCreated ProcessOutcome = "created"
	OutcomeUpdated ProcessOutcome = "updated"
	OutcomeSkipped ProcessOutcome = "skipped"
	OutcomeError   ProcessOutcome = "error"
)

// Summary aggregates outcomes across ingest and daemon processing.
type Summary struct {
	Created int
	Updated int
	Skipped int
	Errors  int
}

// PathBuilder maps provider/session pairs to a markdown output path.
type PathBuilder func(providerID string, session *spi.AgentChatSession) string

// Options configures shared ingest/daemon processing.
type Options struct {
	HistoryDir             string
	StatisticsPath         string
	StateDBPath            string
	UseUTC                 bool
	Debounce               time.Duration
	PathBuilder            PathBuilder
	ShouldProcessSession   func(providerID string, session *spi.AgentChatSession) bool
	OnProviderScanStart    func(providerID string)
	OnProviderScanComplete func(providerID string, totalSessions int, err error)
	// OnSessionProcessed reports each session outcome during ingest. processed
	// and total count provider sources (a session file or project directory),
	// including the one the session came from.
	OnSessionProcessed func(providerID string, outcome ProcessOutcome, processed int, total int)
	// OnSourceProcessed reports progress after each source, including sources
	// that were skipped or produced no sessions.
	OnSourceProcessed func(providerID string, processed int, total int)

	// SkipUnchangedSources makes ingest skip parsing a source whose files are
	// unchanged since its sessions were archived and whose output still exists.
	SkipUnchangedSources bool
	// SourceScope is mixed into source fingerprints. Set it to any setting
	// outside the source files that changes which sessions are archived or how
	// (e.g. project exclusions), so changing the setting re-parses sources.
	SourceScope string
}

type pendingUpdate struct {
	providerID string
	session    *spi.AgentChatSession
	timer      *time.Timer
}

// Engine implements shared historical ingest and incremental watch processing.
type Engine struct {
	opts  Options
	state *StateStore
	stats *sessionpkg.StatisticsCollector

	pendingMu sync.Mutex
	pending   map[string]*pendingUpdate
	closed    bool

	// inFlight tracks debounce-fired flushes that have claimed their pending
	// entry but are still processing; Close must wait for them, because
	// FlushPending can no longer see those updates and closing the state
	// store underneath an in-flight processSession would lose the result.
	inFlight sync.WaitGroup

	processMu sync.Mutex

	summaryMu sync.Mutex
	summary   Summary

	// fingerprintScope covers the engine settings that shape archived output
	fingerprintScope string
}

// New creates a session processing engine backed by a persistent runtime state DB.
func New(opts Options) (*Engine, error) {
	if opts.HistoryDir == "" {
		return nil, fmt.Errorf("history dir is required")
	}
	if opts.StatisticsPath == "" {
		return nil, fmt.Errorf("statistics path is required")
	}
	if opts.StateDBPath == "" {
		return nil, fmt.Errorf("state db path is required")
	}
	if opts.Debounce <= 0 {
		opts.Debounce = defaultDebounce
	}
	if opts.PathBuilder == nil {
		historyDir := opts.HistoryDir
		opts.PathBuilder = func(providerID string, session *spi.AgentChatSession) string {
			timestamp, _ := time.Parse(time.RFC3339, session.CreatedAt)
			timestampStr := formatSessionFilenameTimestamp(timestamp, opts.UseUTC)
			filename := timestampStr
			if session.Slug != "" {
				filename = fmt.Sprintf("%s-%s", timestampStr, session.Slug)
			}
			return filepath.Join(historyDir, providerID, filename+".md")
		}
	}

	state, err := OpenStateStore(opts.StateDBPath)
	if err != nil {
		return nil, err
	}

	// Why the hostname: it is written into each transcript's frontmatter.
	host, _ := os.Hostname()
	return &Engine{
		opts:             opts,
		state:            state,
		stats:            sessionpkg.NewStatisticsCollector(opts.StatisticsPath),
		pending:          make(map[string]*pendingUpdate),
		fingerprintScope: fmt.Sprintf("history=%s utc=%t host=%s scope=%s", opts.HistoryDir, opts.UseUTC, host, opts.SourceScope),
	}, nil
}

// Close flushes pending debounced updates, flushes statistics, and closes state storage.
func (e *Engine) Close() error {
	if err := e.FlushPending(); err != nil {
		return err
	}

	e.pendingMu.Lock()
	e.closed = true
	e.pendingMu.Unlock()

	// Wait for debounce timers that fired before closed was set: they hold
	// updates FlushPending could not see and may still be writing.
	e.inFlight.Wait()

	if err := e.stats.Flush(); err != nil {
		return fmt.Errorf("flush statistics: %w", err)
	}

	if err := e.state.Close(); err != nil {
		return fmt.Errorf("close state store: %w", err)
	}
	return nil
}

// IngestProviders performs a historical ingest pass across providers.
// Providers are streamed concurrently, one source at a time, so memory follows
// the largest source rather than a provider's whole history.
func (e *Engine) IngestProviders(ctx context.Context, projectPath string, providers map[string]spi.Provider, debugRaw bool) (Summary, error) {
	providerIDs := make([]string, 0, len(providers))
	for providerID := range providers {
		providerIDs = append(providerIDs, providerID)
	}
	sort.Strings(providerIDs)

	var summaryMu sync.Mutex
	runSummary := Summary{}
	record := func(outcome ProcessOutcome) {
		summaryMu.Lock()
		defer summaryMu.Unlock()
		switch outcome {
		case OutcomeCreated:
			runSummary.Created++
		case OutcomeUpdated:
			runSummary.Updated++
		case OutcomeSkipped:
			runSummary.Skipped++
		case OutcomeError:
			runSummary.Errors++
		}
	}

	// Why wait for every provider even after cancellation: their callbacks
	// write through the state store, which the caller closes once this returns.
	var wg sync.WaitGroup
	for _, providerID := range providerIDs {
		provider := providers[providerID]
		if e.opts.OnProviderScanStart != nil {
			e.opts.OnProviderScanStart(providerID)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.ingestProvider(ctx, providerID, provider, projectPath, debugRaw, record)
		}()
	}
	wg.Wait()

	summaryMu.Lock()
	defer summaryMu.Unlock()
	return runSummary, ctx.Err()
}

// WatchProviders watches providers and queues incremental updates through debounce processing.
func (e *Engine) WatchProviders(ctx context.Context, projectPath string, providers map[string]spi.Provider, debugRaw bool) error {
	return utils.WatchProviders(ctx, projectPath, providers, debugRaw, func(providerID string, session *spi.AgentChatSession) {
		e.QueueSessionUpdate(providerID, session)
	})
}

// QueueSessionUpdate debounces repeated updates for the same provider/session pair.
func (e *Engine) QueueSessionUpdate(providerID string, session *spi.AgentChatSession) {
	if session == nil {
		return
	}
	if !e.shouldProcessSession(providerID, session) {
		return
	}

	sessionCopy := *session
	key := providerID + ":" + session.SessionID

	e.pendingMu.Lock()
	defer e.pendingMu.Unlock()

	if e.closed {
		return
	}

	if existing, ok := e.pending[key]; ok {
		if existing.timer != nil {
			existing.timer.Stop()
		}
		existing.session = &sessionCopy
		existing.timer = time.AfterFunc(e.opts.Debounce, func() {
			e.flushPendingKey(key)
		})
		return
	}

	update := &pendingUpdate{
		providerID: providerID,
		session:    &sessionCopy,
	}
	update.timer = time.AfterFunc(e.opts.Debounce, func() {
		e.flushPendingKey(key)
	})
	e.pending[key] = update
}

// FlushPending processes all currently queued debounced updates immediately.
func (e *Engine) FlushPending() error {
	e.pendingMu.Lock()
	pending := make([]*pendingUpdate, 0, len(e.pending))
	for key, update := range e.pending {
		if update.timer != nil {
			update.timer.Stop()
		}
		pending = append(pending, update)
		delete(e.pending, key)
	}
	e.pendingMu.Unlock()

	for _, update := range pending {
		if _, err := e.processSession(update.providerID, update.session); err != nil {
			e.recordOutcome(OutcomeError)
			slog.Error("Engine failed to flush pending session",
				"provider", update.providerID,
				"session_id", update.session.SessionID,
				"error", err)
		}
	}
	return nil
}

// SnapshotSummary returns cumulative engine outcomes across all processed sessions.
func (e *Engine) SnapshotSummary() Summary {
	e.summaryMu.Lock()
	defer e.summaryMu.Unlock()
	return e.summary
}

func (e *Engine) flushPendingKey(key string) {
	e.pendingMu.Lock()
	update, ok := e.pending[key]
	if ok {
		delete(e.pending, key)
		// Registered under pendingMu so Close observes either the pending
		// entry (drained by FlushPending) or a nonzero in-flight count —
		// never neither.
		e.inFlight.Add(1)
	}
	e.pendingMu.Unlock()
	if !ok {
		return
	}
	defer e.inFlight.Done()

	if _, err := e.processSession(update.providerID, update.session); err != nil {
		e.recordOutcome(OutcomeError)
		slog.Error("Engine failed to process debounced update",
			"provider", update.providerID,
			"session_id", update.session.SessionID,
			"error", err)
	}
}

func (e *Engine) processSession(providerID string, session *spi.AgentChatSession) (ProcessOutcome, error) {
	if session == nil || session.SessionData == nil {
		return OutcomeError, fmt.Errorf("session or session data is nil")
	}

	e.processMu.Lock()
	defer e.processMu.Unlock()

	filePath := e.opts.PathBuilder(providerID, session)
	if filePath == "" {
		return OutcomeError, fmt.Errorf("empty output path")
	}
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return OutcomeError, fmt.Errorf("create output directory: %w", err)
	}
	unlock, err := sessionpkg.LockTranscript(filePath)
	if err != nil {
		return OutcomeError, err
	}
	defer unlock()

	host, err := os.Hostname()
	if err != nil {
		return OutcomeError, fmt.Errorf("get hostname: %w", err)
	}
	annotations := sessionpkg.Annotations{}
	if existing, readErr := os.ReadFile(filePath); readErr == nil {
		annotations = sessionpkg.ExtractAnnotations(existing)
	}
	metadata := sessionpkg.ApplyAnnotations(sessionpkg.DeriveMetadata(session.SessionData, host), annotations)
	markdownContent, err := sessionpkg.GenerateMarkdownWithMetadata(session.SessionData, metadata, false, e.opts.UseUTC)
	if err != nil {
		return OutcomeError, fmt.Errorf("generate markdown: %w", err)
	}

	contentHash := hashMarkdown(markdownContent)
	existingState, hasState, err := e.state.Get(providerID, session.SessionID)
	if err != nil {
		return OutcomeError, err
	}

	if hasState && existingState.ContentHash == contentHash {
		if data, readErr := os.ReadFile(filePath); readErr == nil && string(data) == markdownContent {
			e.recordOutcome(OutcomeSkipped)
			return OutcomeSkipped, nil
		}
	}

	fileExists := false
	if _, statErr := os.Stat(filePath); statErr == nil {
		fileExists = true
	}

	temporaryPath := filePath + ".tmp"
	if err := os.WriteFile(temporaryPath, []byte(markdownContent), 0o644); err != nil {
		return OutcomeError, fmt.Errorf("write markdown: %w", err)
	}
	if err := os.Rename(temporaryPath, filePath); err != nil {
		_ = os.Remove(temporaryPath)
		return OutcomeError, fmt.Errorf("replace markdown: %w", err)
	}

	if err := e.state.Upsert(SessionState{
		ProviderID:  providerID,
		SessionID:   session.SessionID,
		ContentHash: contentHash,
		OutputPath:  filePath,
		UpdatedAt:   time.Now().UTC(),
	}); err != nil {
		return OutcomeError, err
	}

	if err := e.saveStatistics(providerID, session, markdownContent); err != nil {
		return OutcomeError, err
	}

	if fileExists {
		e.recordOutcome(OutcomeUpdated)
		return OutcomeUpdated, nil
	}
	e.recordOutcome(OutcomeCreated)
	return OutcomeCreated, nil
}

// ProcessSession writes one session to the archive using the same path, state,
// and statistics behavior as ingest and watch processing.
func (e *Engine) ProcessSession(providerID string, session *spi.AgentChatSession) (ProcessOutcome, error) {
	if !e.shouldProcessSession(providerID, session) {
		return OutcomeSkipped, nil
	}
	return e.processSession(providerID, session)
}

func (e *Engine) saveStatistics(providerID string, session *spi.AgentChatSession, markdownContent string) error {
	stats := sessionpkg.ComputeSessionStatistics(session.SessionData, markdownContent, providerID)
	e.stats.AddSessionStats(session.SessionID, stats)
	return nil
}

func (e *Engine) recordOutcome(outcome ProcessOutcome) {
	e.summaryMu.Lock()
	defer e.summaryMu.Unlock()

	switch outcome {
	case OutcomeCreated:
		e.summary.Created++
	case OutcomeUpdated:
		e.summary.Updated++
	case OutcomeSkipped:
		e.summary.Skipped++
	case OutcomeError:
		e.summary.Errors++
	}
}

func hashMarkdown(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func (e *Engine) shouldProcessSession(providerID string, session *spi.AgentChatSession) bool {
	if e.opts.ShouldProcessSession == nil {
		return true
	}
	return e.opts.ShouldProcessSession(providerID, session)
}

func formatSessionFilenameTimestamp(t time.Time, useUTC bool) string {
	if useUTC {
		return t.UTC().Format("2006-01-02_15-04-05") + "Z"
	}
	return t.Local().Format("2006-01-02_15-04-05-0700")
}
