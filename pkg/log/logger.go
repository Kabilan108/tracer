package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tracer-ai/tracer-cli/pkg/ui"
)

// Logger configuration
var (
	logger        *slog.Logger
	logFileHandle *os.File
)

// UserMessage prints a plain message to stderr without any prefix or color
// This respects the silent flag - no output if silent mode is enabled
func UserMessage(format string, args ...interface{}) {
	if !isSilent() {
		message := fmt.Sprintf(format, args...)
		fmt.Fprint(os.Stderr, message)
	}
}

// UserWarn prints a warning message to stderr.
// This respects the silent flag - no output if silent mode is enabled
func UserWarn(format string, args ...interface{}) {
	if !isSilent() {
		message := fmt.Sprintf(format, args...)
		fmt.Fprintf(os.Stderr, "\n%s %s\n", ui.Warning("Warning"), message)
	}
}

// UserError prints an error message to stderr.
// This respects the silent flag - no output if silent mode is enabled
func UserError(format string, args ...interface{}) {
	if !isSilent() {
		message := fmt.Sprintf(format, args...)
		fmt.Fprintf(os.Stderr, "\n%s %s\n", ui.Error("Error"), message)
	}
}

// SetupLogger configures slog based on flags
// console: enables logging to stdout
// logFile: enables logging to file
// logPath: path to the log file (required if logFile is true)
// debug: changes log level to Debug (only valid with console or logFile)
func SetupLogger(console, logFile, debug bool, logPath string) error {
	var handlers []slog.Handler

	// Determine log level
	logLevel := slog.LevelInfo
	if debug {
		logLevel = slog.LevelDebug
	}

	// Add file handler if --log flag is set
	if logFile {
		// Create directory if it doesn't exist
		dir := filepath.Dir(logPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create log directory: %v", err)
		}

		// Open log file for append
		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("failed to open log file: %v", err)
		}
		logFileHandle = file

		handlers = append(handlers, slog.NewTextHandler(file, &slog.HandlerOptions{
			Level: logLevel,
		}))
	}

	// Add stdout handler if --console flag is set
	if console {
		handlers = append(handlers, slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: logLevel,
		}))
	}

	// Create logger based on handlers
	switch len(handlers) {
	case 0:
		// No logging - discard everything
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	case 1:
		// Single handler
		logger = slog.New(handlers[0])
	default:
		// Multiple handlers
		logger = slog.New(&multiHandler{handlers: handlers})
	}

	// Set as default logger
	slog.SetDefault(logger)
	return nil
}

// CloseLogger closes the log file if open
func CloseLogger() {
	if logFileHandle != nil {
		_ = logFileHandle.Close()
		logFileHandle = nil
	}
}

// multiHandler allows writing to multiple destinations
type multiHandler struct {
	handlers []slog.Handler
}

func (h *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, handler := range h.handlers {
		if err := handler.Handle(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

func (h *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newHandlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		newHandlers[i] = handler.WithAttrs(attrs)
	}
	return &multiHandler{handlers: newHandlers}
}

func (h *multiHandler) WithGroup(name string) slog.Handler {
	newHandlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		newHandlers[i] = handler.WithGroup(name)
	}
	return &multiHandler{handlers: newHandlers}
}

// silentMode tracks whether silent mode is enabled (set during logger setup)
var silentMode bool

// isSilent checks if silent mode is enabled (internal use)
func isSilent() bool {
	return silentMode
}

// IsSilent returns whether silent mode is enabled (for external packages)
func IsSilent() bool {
	return silentMode
}

// SetSilent sets the silent mode flag
func SetSilent(silent bool) {
	silentMode = silent
}
