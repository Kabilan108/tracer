package engine

import (
	"context"

	"github.com/specstoryai/getspecstory/specstory-cli/pkg/spi"
)

// RunIngest executes a one-shot historical ingest using the shared engine.
func RunIngest(ctx context.Context, opts Options, projectPath string, providers map[string]spi.Provider, debugRaw bool) (Summary, error) {
	engine, err := New(opts)
	if err != nil {
		return Summary{}, err
	}

	summary, err := engine.IngestProviders(ctx, projectPath, providers, debugRaw)
	closeErr := engine.Close()
	if err != nil {
		return summary, err
	}
	if closeErr != nil {
		return summary, closeErr
	}
	return summary, nil
}

// RunDaemon executes an initial ingest, then watches for incremental updates until ctx is canceled.
func RunDaemon(ctx context.Context, opts Options, projectPath string, providers map[string]spi.Provider, debugRaw bool) (Summary, error) {
	engine, err := New(opts)
	if err != nil {
		return Summary{}, err
	}

	if _, err := engine.IngestProviders(ctx, projectPath, providers, debugRaw); err != nil {
		_ = engine.Close()
		return engine.SnapshotSummary(), err
	}

	if err := engine.WatchProviders(ctx, projectPath, providers, debugRaw); err != nil && ctx.Err() == nil {
		_ = engine.Close()
		return engine.SnapshotSummary(), err
	}

	if err := engine.Close(); err != nil {
		return engine.SnapshotSummary(), err
	}
	return engine.SnapshotSummary(), nil
}
