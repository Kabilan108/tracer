package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/tracer-ai/tracer-cli/pkg/spi"
)

// SourceFingerprintVersion identifies the parsing and rendering behavior that
// produced archived output from a source.
//
// Why: watch startup skips a source whose files are unchanged since its
// sessions were archived. That is only correct while re-parsing those files
// would render the same markdown. Bump this constant in any change that can
// alter generated output: provider JSONL parsing, SessionData generation, tool
// formatting, markdown or frontmatter rendering, or archive paths. The next
// watch startup then re-parses every source once. `tracer sync` never skips,
// so it remains the way to rebuild the archive if a bump was missed.
const SourceFingerprintVersion = 1

// providerIngest tracks one provider's streaming ingest. Visitor callbacks
// arrive sequentially from the provider's goroutine, so its fields need no lock;
// shared engine state is reached through methods that lock on their own.
type providerIngest struct {
	ctx         context.Context
	engine      *Engine
	providerID  string
	projectPath string
	debugRaw    bool
	record      func(ProcessOutcome)

	sourcesReported bool
	total           int
	processed       int
	parsedSources   int
	skippedSources  int

	// Per-source accumulation, reset when the source is done
	inSource   bool
	sessionIDs []string
	failed     bool
}

// ingestProvider streams one provider's history through the engine.
func (e *Engine) ingestProvider(ctx context.Context, providerID string, provider spi.Provider, projectPath string, debugRaw bool, record func(ProcessOutcome)) {
	streamer, ok := provider.(spi.SessionStreamer)
	if !ok {
		streamer = sliceStreamer{provider: provider}
	}

	run := &providerIngest{
		ctx:         ctx,
		engine:      e,
		providerID:  providerID,
		projectPath: projectPath,
		debugRaw:    debugRaw,
		record:      record,
	}
	start := time.Now()
	err := streamer.StreamAgentChatSessions(ctx, projectPath, debugRaw, spi.SessionVisitor{
		Sources:     run.sources,
		ShouldParse: run.shouldParse,
		Session:     run.session,
		Done:        run.done,
	})
	slog.Info("Engine ingest provider complete",
		"provider", providerID,
		"sources", run.total,
		"parsed_sources", run.parsedSources,
		"skipped_sources", run.skippedSources,
		"skip_unchanged", e.opts.SkipUnchangedSources,
		"duration", time.Since(start),
		"error", err)
	if err == nil || ctx.Err() != nil {
		return
	}

	if !run.sourcesReported && e.opts.OnProviderScanComplete != nil {
		e.opts.OnProviderScanComplete(providerID, 0, err)
	}
	record(OutcomeError)
	e.recordOutcome(OutcomeError)
	slog.Error("Engine ingest failed to list sessions", "provider", providerID, "error", err)
}

func (r *providerIngest) sources(total int) {
	r.sourcesReported = true
	r.total = total
	if r.engine.opts.OnProviderScanComplete != nil {
		r.engine.opts.OnProviderScanComplete(r.providerID, total, nil)
	}
}

func (r *providerIngest) shouldParse(src spi.SessionSource) bool {
	r.processed++
	sessionIDs, unchanged := r.engine.sourceUnchanged(r.providerID, r.projectPath, src, r.debugRaw)
	if !unchanged {
		r.inSource = true
		r.parsedSources++
		return true
	}
	r.skippedSources++

	slog.Debug("Engine ingest skipped unchanged source",
		"provider", r.providerID,
		"source", src.Key,
		"sessions", len(sessionIDs))
	for range sessionIDs {
		r.record(OutcomeSkipped)
		r.engine.recordOutcome(OutcomeSkipped)
		r.reportSession(OutcomeSkipped)
	}
	r.reportSource()
	return false
}

func (r *providerIngest) session(_ spi.SessionSource, session *spi.AgentChatSession) {
	e := r.engine
	if r.ctx.Err() != nil {
		// Providers only check for cancellation between sources, and one
		// Claude project can hold hundreds of sessions; stop writing now.
		r.failed = true
		return
	}
	if !e.shouldProcessSession(r.providerID, session) {
		r.record(OutcomeSkipped)
		e.recordOutcome(OutcomeSkipped)
		r.reportSession(OutcomeSkipped)
		return
	}

	outcome, err := e.processSession(r.providerID, session)
	if err != nil {
		r.failed = true
		outcome = OutcomeError
		e.recordOutcome(OutcomeError)
		slog.Error("Engine ingest failed to process session",
			"provider", r.providerID,
			"session_id", session.SessionID,
			"error", err)
	} else {
		r.sessionIDs = append(r.sessionIDs, session.SessionID)
	}
	r.record(outcome)
	r.reportSession(outcome)
}

func (r *providerIngest) done(src spi.SessionSource, err error) {
	if !r.inSource {
		// The provider reported a source it could not even stat
		r.processed++
	}

	switch {
	case err != nil:
		// Parse failures are logged by the provider; leaving the source
		// unrecorded makes the next startup retry it.
		slog.Debug("Engine ingest source failed", "provider", r.providerID, "source", src.Key, "error", err)
	case r.failed:
		// Some sessions were not archived; retry the source next startup.
	default:
		r.engine.recordSource(r.providerID, r.projectPath, src, r.sessionIDs)
	}

	r.inSource = false
	r.sessionIDs = nil
	r.failed = false
	r.reportSource()
}

func (r *providerIngest) reportSession(outcome ProcessOutcome) {
	if r.engine.opts.OnSessionProcessed != nil {
		r.engine.opts.OnSessionProcessed(r.providerID, outcome, r.processed, r.total)
	}
}

func (r *providerIngest) reportSource() {
	if r.engine.opts.OnSourceProcessed != nil {
		r.engine.opts.OnSourceProcessed(r.providerID, r.processed, r.total)
	}
}

// sourceUnchanged reports whether src can be skipped: skipping is enabled, the
// source's files and the fingerprint scope match what was recorded when its
// sessions were archived, and every archived output still exists.
func (e *Engine) sourceUnchanged(providerID string, projectPath string, src spi.SessionSource, debugRaw bool) ([]string, bool) {
	// Why debugRaw disables skipping: it asks for debug files of every session,
	// which only a parse produces.
	if !e.opts.SkipUnchangedSources || debugRaw || src.Key == "" {
		return nil, false
	}

	state, ok, err := e.state.GetSource(providerID, src.Key)
	if err != nil {
		slog.Warn("Engine failed to read source state", "provider", providerID, "source", src.Key, "error", err)
		return nil, false
	}
	if !ok || state.Version != SourceFingerprintVersion || state.Fingerprint != e.sourceFingerprint(projectPath, src) {
		return nil, false
	}

	for _, sessionID := range state.SessionIDs {
		sessionState, ok, err := e.state.Get(providerID, sessionID)
		if err != nil || !ok {
			return nil, false
		}
		if _, err := os.Stat(sessionState.OutputPath); err != nil {
			return nil, false
		}
	}
	return state.SessionIDs, true
}

// recordSource stores the fingerprint of a fully archived source.
func (e *Engine) recordSource(providerID string, projectPath string, src spi.SessionSource, sessionIDs []string) {
	if src.Key == "" {
		return
	}
	err := e.state.UpsertSource(SourceState{
		ProviderID:  providerID,
		SourceKey:   src.Key,
		Fingerprint: e.sourceFingerprint(projectPath, src),
		Version:     SourceFingerprintVersion,
		SessionIDs:  sessionIDs,
	})
	if err != nil {
		slog.Warn("Engine failed to record source state", "provider", providerID, "source", src.Key, "error", err)
	}
}

// sourceFingerprint hashes everything that determines a source's archived
// output besides the parser version: the stat of each of its files, and the
// engine settings that change which sessions are written or how.
// Why stat and not content: hashing content means reading every file, which
// is the startup cost being avoided. Agent transcripts are append-only, so a
// content change without a size, mtime or inode change does not happen in
// practice.
func (e *Engine) sourceFingerprint(projectPath string, src spi.SessionSource) string {
	files := append([]spi.FileStat(nil), src.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	hash := sha256.New()
	writeField := func(value string) {
		_, _ = hash.Write([]byte(strconv.Itoa(len(value))))
		_, _ = hash.Write([]byte{':'})
		_, _ = hash.Write([]byte(value))
	}
	writeField(e.fingerprintScope)
	writeField(projectPath)
	for _, file := range files {
		writeField(file.Path)
		writeField(fmt.Sprintf("%d/%d/%d/%d", file.Size, file.ModTime, file.Dev, file.Inode))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// sliceStreamer adapts a provider without spi.SessionStreamer. Each session is
// its own source without files, so it is always parsed and never fingerprinted.
type sliceStreamer struct {
	provider spi.Provider
}

func (s sliceStreamer) StreamAgentChatSessions(ctx context.Context, projectPath string, debugRaw bool, visitor spi.SessionVisitor) error {
	sessions, err := s.provider.GetAgentChatSessions(projectPath, debugRaw, nil)
	if err != nil {
		return err
	}
	visitor.ReportSources(len(sessions))

	for i := range sessions {
		if err := ctx.Err(); err != nil {
			return err
		}
		src := spi.SessionSource{}
		if !visitor.ParseNeeded(src) {
			continue
		}
		visitor.ReportSession(src, &sessions[i])
		visitor.ReportDone(src, nil)
	}
	return nil
}
