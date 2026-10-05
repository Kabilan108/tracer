package spi

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/tracer-ai/tracer-cli/pkg/spi/schema"
)

// CheckResult contains the result of a provider check operation
type CheckResult struct {
	Success      bool   // Whether the check succeeded
	Version      string // Version of the provider (empty on failure)
	Location     string // File path/location of the provider executable
	ErrorMessage string // Error message if check failed (empty on success)
}

// ProgressCallback reports progress during session parsing/processing
// current: number of items processed so far (1-based)
// total: total number of items to process
type ProgressCallback func(current, total int)

// AgentChatSession represents a chat session from an AI coding agent
type AgentChatSession struct {
	SessionID   string              // Stable and unique identifier for the session (often a UUID)
	CreatedAt   string              // Stable ISO 8601 timestamp when session was created
	Slug        string              // Stable human-readable but file name safe slug, often derived from first user message
	SessionData *schema.SessionData // Structured session data in unified format
}

// SessionMetadata contains lightweight metadata about a session without full content
// Used by ListAgentChatSessions for efficient session listing
type SessionMetadata struct {
	SessionID     string `json:"session_id" csv:"session_id"`         // Stable and unique identifier for the session
	CreatedAt     string `json:"created_at" csv:"created_at"`         // Stable ISO 8601 timestamp when session was created
	Slug          string `json:"slug" csv:"slug"`                     // Stable human-readable session name/slug
	Name          string `json:"name" csv:"name"`                     // Human-readable description of the session (may be empty if not available)
	WorkspaceRoot string `json:"workspace_root" csv:"workspace_root"` // Absolute project path for the session when known
}

// Provider defines the interface that all agent coding tool providers must implement
type Provider interface {
	// Name returns the human-readable name of the provider (e.g., "Claude Code", "Cursor CLI", "Codex CLI")
	Name() string

	// Check verifies if the provider is properly installed and returns version info
	// customCommand: empty string = use detected/default binary path, non-empty = use this specific command/path
	Check(customCommand string) CheckResult

	// DetectAgent checks if the provider's agent has been used in the given project path
	// projectPath: Agent's working directory
	// helpOutput: if true AND no activity is found, the provider should output helpful guidance
	// Returns true if the agent has created artifacts/sessions in the specified path
	DetectAgent(projectPath string, helpOutput bool) bool

	// GetAgentChatSessions retrieves all chat sessions for the given project path
	// projectPath: Agent's working directory
	// debugRaw: if true, provider should write provider-specific raw debug files to ~/.local/state/tracer/debug/<sessionID>/
	//           (e.g., numbered JSON files). The unified session-data.json is written centrally by the CLI.
	// progress: optional callback for reporting progress during parsing (nil = no progress reporting)
	// Returns a slice of AgentChatSession structs containing session data
	GetAgentChatSessions(projectPath string, debugRaw bool, progress ProgressCallback) ([]AgentChatSession, error)

	// GetAgentChatSession retrieves one fully parsed chat session by ID.
	// projectPath: Agent's working directory; empty means search all known projects/sessions.
	// Returns nil, nil when the provider can scan successfully but does not contain the session.
	GetAgentChatSession(projectPath string, sessionID string, debugRaw bool) (*AgentChatSession, error)

	// ListAgentChatSessions retrieves lightweight metadata for all sessions without full parsing
	// This should be faster than GetAgentChatSessions as it only needs to return SessionMetadata and not the full AgentChatSession
	// projectPath: Agent's working directory
	// Returns a slice of SessionMetadata (ordering is provider-defined; consumers will sort if needed)
	ListAgentChatSessions(projectPath string) ([]SessionMetadata, error)

	// WatchAgent watches for agent activity and calls the callback with an updated AgentChatSession
	// Does NOT execute the agent - only watches for new activity
	// Runs until error or context cancellation
	// ctx: Context for cancellation and timeout control
	// projectPath: Agent's working directory
	// debugRaw: if true, provider should write provider-specific raw debug files to ~/.local/state/tracer/debug/<sessionID>/
	//           (e.g., numbered JSON files). The unified session-data.json is written centrally by the CLI.
	// sessionCallback: called with AgentChatSession on each update (provider should not block on callback)
	// The implementation should handle its own file watching and session tracking
	WatchAgent(ctx context.Context, projectPath string, debugRaw bool, sessionCallback func(*AgentChatSession)) error
}

// SessionSource is the smallest set of files a provider parses as a whole.
// Sessions parsed from a source depend only on the files listed in it, so an
// unchanged source yields unchanged sessions.
type SessionSource struct {
	Key   string     // Stable identifier, e.g. a session file or project directory path
	Files []FileStat // Every file the source's sessions are parsed from, stat'ed before parsing
}

// SessionVisitor receives sessions from SessionStreamer one source at a time.
// Callbacks are invoked sequentially from the streaming goroutine.
type SessionVisitor struct {
	// Sources is called once, before any source is parsed, with the number of sources found.
	Sources func(total int)
	// ShouldParse reports whether src must be parsed. Returning false skips it without reading its files.
	ShouldParse func(src SessionSource) bool
	// Session receives each session parsed from src.
	Session func(src SessionSource, session *AgentChatSession)
	// Done is called after the last session of a parsed src, with the parse error if any.
	Done func(src SessionSource, err error)
}

// SessionStreamer is implemented by providers that can stream historical sessions
// instead of returning them all at once from GetAgentChatSessions.
// Why: holding every parsed session of a provider in one slice made peak memory
// scale with the whole history; streaming bounds it by the largest source.
type SessionStreamer interface {
	// StreamAgentChatSessions parses sources for projectPath (empty = all projects) and
	// reports them to visitor. It stops between sources when ctx is cancelled.
	StreamAgentChatSessions(ctx context.Context, projectPath string, debugRaw bool, visitor SessionVisitor) error
}

// CatchUpWatcher is implemented by providers whose watcher can run a catch-up
// pass between registering its watches and handling its first event.
// Why: a write made after historical ingest read a source but before the
// watcher watched it produces no event, so it would never reach the archive.
// Running catchUp once every existing source is watched closes that gap, and
// because events arriving meanwhile are handled only after catchUp returns, a
// catch-up parse never lands after a newer watch parse of the same session.
type CatchUpWatcher interface {
	// WatchAgentWithCatchUp behaves like WatchAgent and calls catchUp, when
	// set, synchronously once before processing any event, and again through
	// RecoverFromOverflow whenever the watcher's event queue overflows.
	WatchAgentWithCatchUp(ctx context.Context, projectPath string, debugRaw bool, catchUp func(), sessionCallback func(*AgentChatSession)) error
}

// ErrNothingToWatch is returned, wrapped, by a provider watcher when the
// provider has no session data on this machine, such as a missing sessions
// directory.
// Why a sentinel: any other watcher error stops every provider's watcher, so
// that a supervisor restarts the whole service instead of leaving it running
// with one provider dead. A machine that uses only one of the agents must
// still be able to watch that one.
var ErrNothingToWatch = errors.New("nothing to watch")

// RecoverFromOverflow brings a watcher whose event queue overflowed back in
// step with its sources, without stopping it.
//
// Why: the kernel dropped events, so files written and directories created
// in the meantime are unknown to the watcher, and a session whose last write
// fell in that window would stay unarchived until it is written again.
// rewatch registers every directory that should be watched, before the
// rescan, so any later write produces an event: directories that appeared,
// and directories replaced at the same path, whose watch ended with the
// directory it was on (see DirWatches.Reset). The throttle is flushed so
// that no run scheduled before the overflow delivers its snapshot after the
// rescan's.
// catchUp then parses every source changed since it was last archived, as at
// startup, and events that queue up meanwhile are handled after it returns,
// so their parses land after the rescan's, as at startup.
//
// It must be called from the goroutine that handles the watcher's events.
func RecoverFromOverflow(watcher string, throttle *FileThrottle, rewatch func(), catchUp func()) {
	started := time.Now()
	rewatch()
	throttle.Flush()
	catchUp()
	slog.Warn("Watcher rescanned sources after its event queue overflowed",
		"watcher", watcher,
		"reason", "events were dropped, so changes made meanwhile may have had no event",
		"duration", time.Since(started))
}

// DirWatches adds directories to an fsnotify watcher once each, remembering
// which ones it added. It is not safe for concurrent use; call it from the
// goroutine that handles the watcher's events.
type DirWatches struct {
	watcher *fsnotify.Watcher
	added   map[string]bool
}

// NewDirWatches returns an empty DirWatches for watcher.
func NewDirWatches(watcher *fsnotify.Watcher) *DirWatches {
	return &DirWatches{watcher: watcher, added: make(map[string]bool)}
}

// Add watches dir unless it was already added, and reports whether it added it.
// Why skip: watchers walk their whole tree again whenever a directory
// appears, and re-adding every directory each time would be wasted work.
func (d *DirWatches) Add(dir string) (bool, error) {
	if d.added[dir] {
		return false, nil
	}
	if err := d.watcher.Add(dir); err != nil {
		return false, err
	}
	d.added[dir] = true
	return true, nil
}

// Forget drops path and every directory under it, so they are added again.
// Call it when path is removed or renamed.
// Why: a watch is on the directory, not its path. Once it is removed or
// moved away, a directory created at the same path gets no watch unless it
// is added again, and directories under a moved one keep watching their
// moved copies.
func (d *DirWatches) Forget(path string) {
	prefix := path + string(filepath.Separator)
	maps.DeleteFunc(d.added, func(dir string, _ bool) bool {
		return dir == path || strings.HasPrefix(dir, prefix)
	})
}

// Reset forgets every directory, so the next walk adds each one again.
// Call it after the event queue overflowed.
// Why: the events that tell of a directory's removal can be among those
// dropped, and then Forget never ran for it. fsnotify's own WatchList is no
// better, because it too only learns of a removal from those events.
// Adding a path again watches whatever directory is there now, and does
// nothing to a watch that is still live.
func (d *DirWatches) Reset() {
	clear(d.added)
}

// ReportSources calls Sources when set.
func (v SessionVisitor) ReportSources(total int) {
	if v.Sources != nil {
		v.Sources(total)
	}
}

// ParseNeeded calls ShouldParse when set; without it every source is parsed.
func (v SessionVisitor) ParseNeeded(src SessionSource) bool {
	return v.ShouldParse == nil || v.ShouldParse(src)
}

// ReportSession calls Session when set.
func (v SessionVisitor) ReportSession(src SessionSource, session *AgentChatSession) {
	if v.Session != nil {
		v.Session(src, session)
	}
}

// ReportDone calls Done when set.
func (v SessionVisitor) ReportDone(src SessionSource, err error) {
	if v.Done != nil {
		v.Done(src, err)
	}
}
