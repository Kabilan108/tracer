package utils

import (
	"context"
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
// Runs until context is cancelled, a watcher fails, or all watchers stop.
// Context cancellation (Ctrl+C) is treated as a clean exit, not an error.
//
// A watcher error stops every other watcher and is returned. Why: a daemon
// left running with one provider's watcher dead stops archiving that provider
// silently, while exiting lets a supervisor such as systemd restart it whole.
// The exception is spi.ErrNothingToWatch, a provider with nothing on this
// machine to watch; the other watchers keep running.
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

	parentCtx := ctx
	ctx, stopAll := context.WithCancel(parentCtx)
	defer stopAll()

	var wg sync.WaitGroup
	errChan := make(chan error, len(providers))

	for providerID, provider := range providers {
		providerID := providerID
		provider := provider
		wg.Add(1)
		go func() {
			defer wg.Done()

			slog.Info("WatchProviders: Starting watcher for provider", "providerID", providerID, "providerName", provider.Name())

			// Unchanged sessions are not deduplicated here: the engine compares
			// a hash of the rendered markdown before writing, and hashing the
			// whole session on every callback duplicated that work.
			wrappedCallback := func(session *spi.AgentChatSession) {
				if session == nil || session.SessionData == nil {
					return
				}

				slog.Debug("WatchProviders: Provider callback fired",
					"providerID", providerID,
					"sessionID", session.SessionID)

				sessionCallback(providerID, session)
			}

			err := provider.WatchAgent(ctx, projectPath, debugRaw, wrappedCallback)
			switch {
			case err == nil:
				errChan <- nil
			case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
				// Expected on Ctrl+C, or when another watcher failed
				slog.Info("WatchProviders: Provider watcher stopped", "provider", provider.Name())
				errChan <- nil
			case errors.Is(err, spi.ErrNothingToWatch):
				slog.Warn("WatchProviders: Provider has nothing to watch", "provider", provider.Name(), "error", err)
				errChan <- fmt.Errorf("%s: %w", provider.Name(), err)
			default:
				slog.Error("WatchProviders: Provider watcher failed; stopping all watchers", "provider", provider.Name(), "error", err)
				stopAll()
				errChan <- fmt.Errorf("%s: %w", provider.Name(), err)
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

	if parentCtx.Err() != nil {
		return nil
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
