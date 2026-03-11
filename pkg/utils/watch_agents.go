package utils

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/tracer-ai/tracer-cli/pkg/spi"
	"github.com/tracer-ai/tracer-cli/pkg/spi/factory"
)

// WatchAgents starts watchers for all registered providers concurrently.
// Convenience wrapper around WatchProviders that resolves the full provider registry.
func WatchAgents(ctx context.Context, projectPath string, debugRaw bool, sessionCallback func(providerID string, session *spi.AgentChatSession)) error {
	registry := factory.GetRegistry()
	providers := registry.GetAll()

	if len(providers) == 0 {
		return fmt.Errorf("no providers registered")
	}

	return WatchProviders(ctx, projectPath, providers, debugRaw, sessionCallback)
}

// WatchProviders starts watchers for the given providers concurrently.
// Calls sessionCallback when any provider detects activity.
// Runs until context is cancelled or all watchers stop.
// Context cancellation (Ctrl+C) is treated as a clean exit, not an error.
//
// Parameters:
//   - ctx: Context for cancellation and timeout control
//   - projectPath: Agent's working directory to watch
//   - providers: map of provider ID to provider instance to watch
//   - debugRaw: whether to write debug raw data files
//   - sessionCallback: called with provider ID and AgentChatSession data on each update
//
// The callback includes the provider ID to help consumers route/filter sessions.
// The callback should not block as it may delay other provider notifications.
func WatchProviders(ctx context.Context, projectPath string, providers map[string]spi.Provider, debugRaw bool, sessionCallback func(providerID string, session *spi.AgentChatSession)) error {
	slog.Info("WatchProviders: Starting multi-provider watch", "projectPath", projectPath, "providerCount", len(providers), "debugRaw", debugRaw)

	var mu sync.Mutex
	lastFingerprints := make(map[string]string)

	var wg sync.WaitGroup
	errChan := make(chan error, len(providers))

	for providerID, provider := range providers {
		providerID := providerID
		provider := provider
		wg.Add(1)
		go func() {
			defer wg.Done()

			slog.Info("WatchProviders: Starting watcher for provider", "providerID", providerID, "providerName", provider.Name())

			// Wrap the callback to deduplicate and include provider ID
			wrappedCallback := func(session *spi.AgentChatSession) {
				if session == nil || session.SessionData == nil {
					return
				}

				fingerprint := sessionFingerprint(session)

				// Skip if the session content fingerprint hasn't changed.
				mu.Lock()
				prev, seen := lastFingerprints[providerID+":"+session.SessionID]
				if seen && prev == fingerprint {
					mu.Unlock()
					slog.Debug("WatchProviders: Skipping duplicate callback",
						"providerID", providerID,
						"sessionID", session.SessionID)
					return
				}
				lastFingerprints[providerID+":"+session.SessionID] = fingerprint
				mu.Unlock()

				slog.Debug("WatchProviders: Provider callback fired",
					"providerID", providerID,
					"sessionID", session.SessionID)

				sessionCallback(providerID, session)
			}

			err := provider.WatchAgent(ctx, projectPath, debugRaw, wrappedCallback)
			if err != nil {
				// Context cancellation is expected when user presses Ctrl+C, not an error
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					slog.Info("WatchProviders: Provider watcher stopped", "provider", provider.Name())
					errChan <- nil
				} else {
					slog.Error("WatchProviders: Provider watcher failed", "provider", provider.Name(), "error", err)
					errChan <- fmt.Errorf("%s: %w", provider.Name(), err)
				}
			} else {
				errChan <- nil
			}
		}()
	}

	go func() {
		wg.Wait()
		close(errChan)
	}()

	var errs []error
	for err := range errChan {
		if err != nil {
			errs = append(errs, err)
		}
	}

	if ctx.Err() != nil {
		return nil
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func sessionFingerprint(session *spi.AgentChatSession) string {
	if session == nil {
		return ""
	}

	content := session.RawData
	if session.SessionData != nil {
		if data, err := json.Marshal(session.SessionData); err == nil {
			content += string(data)
		} else {
			content += session.SessionData.UpdatedAt
		}
	}

	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
