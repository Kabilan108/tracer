// Package telemetry provides OpenTelemetry initialization for the Tracer CLI.
// It follows an idempotent-init pattern: first call to Init wins, and the
// disabled path uses the OTel no-op provider.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

var (
	logOnce sync.Once
	logger  *slog.Logger
)

func telemetryLogger() *slog.Logger {
	logOnce.Do(func() {
		logger = slog.Default().With("component", "telemetry")
	})
	return logger
}

const metricExportInterval = 10 * time.Second

type Options struct {
	ServiceName string
	Endpoint    string
	Enabled     bool
}

var (
	initOnce      sync.Once
	traceProvider *sdktrace.TracerProvider
	meterProvider *sdkmetric.MeterProvider
)

func parseEndpoint(raw string) (host string, insecure bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw, true
	}

	host = u.Host
	insecure = u.Scheme != "https"
	return host, insecure
}

func Init(ctx context.Context, opts Options) error {
	var initErr error

	initOnce.Do(func() {
		if !opts.Enabled {
			telemetryLogger().Info("Telemetry disabled, using no-op provider")
			return
		}

		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
			telemetryLogger().Warn("OTel SDK error", "error", err)
		}))

		serviceName := opts.ServiceName
		if serviceName == "" {
			serviceName = "tracer-cli"
		}

		host, insecure := parseEndpoint(opts.Endpoint)
		res, err := resource.New(ctx,
			resource.WithFromEnv(),
			resource.WithTelemetrySDK(),
			resource.WithHost(),
			resource.WithAttributes(attribute.String("service.name", serviceName)),
		)
		if err != nil {
			initErr = fmt.Errorf("create OTel resource: %w", err)
			return
		}

		if err := initTracing(ctx, host, insecure, res); err != nil {
			initErr = err
			return
		}

		if err := initMetrics(ctx, host, insecure, res); err != nil {
			telemetryLogger().Warn("Metrics init failed, rolling back tracing", "error", err)
			if traceProvider != nil {
				if shutdownErr := traceProvider.Shutdown(ctx); shutdownErr != nil {
					telemetryLogger().Warn("Failed to shutdown trace provider during rollback", "error", shutdownErr)
				}
				traceProvider = nil
				otel.SetTracerProvider(noop.NewTracerProvider())
			}
			initErr = err
			return
		}

		telemetryLogger().Info("Telemetry initialised",
			"endpoint", host,
			"insecure", insecure,
			"serviceName", serviceName,
		)
	})

	return initErr
}

func initTracing(ctx context.Context, host string, insecure bool, res *resource.Resource) error {
	exporterOpts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(host),
	}
	if insecure {
		exporterOpts = append(exporterOpts, otlptracegrpc.WithInsecure())
	}

	exporter, err := otlptracegrpc.New(ctx, exporterOpts...)
	if err != nil {
		return fmt.Errorf("create OTLP trace exporter: %w", err)
	}

	traceProvider = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(traceProvider)

	return nil
}

func initMetrics(ctx context.Context, host string, insecure bool, res *resource.Resource) error {
	exporterOpts := []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpoint(host),
	}
	if insecure {
		exporterOpts = append(exporterOpts, otlpmetricgrpc.WithInsecure())
	}

	exporter, err := otlpmetricgrpc.New(ctx, exporterOpts...)
	if err != nil {
		return fmt.Errorf("create OTLP metric exporter: %w", err)
	}

	meterProvider = sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter,
			sdkmetric.WithInterval(metricExportInterval),
		)),
	)
	otel.SetMeterProvider(meterProvider)

	return nil
}

func Shutdown(ctx context.Context) error {
	var errs []error

	if traceProvider != nil {
		telemetryLogger().Info("Flushing and shutting down trace provider")
		if err := traceProvider.ForceFlush(ctx); err != nil {
			telemetryLogger().Warn("Failed to flush trace provider", "error", err)
		}
		if err := traceProvider.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown trace provider: %w", err))
		}
	}

	if meterProvider != nil {
		telemetryLogger().Info("Flushing and shutting down meter provider")
		if err := meterProvider.ForceFlush(ctx); err != nil {
			telemetryLogger().Warn("Failed to flush meter provider", "error", err)
		}
		if err := meterProvider.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown meter provider: %w", err))
		}
	}

	return errors.Join(errs...)
}

func ForceFlush(ctx context.Context) error {
	var errs []error

	if traceProvider != nil {
		telemetryLogger().Debug("Force flushing trace provider")
		if err := traceProvider.ForceFlush(ctx); err != nil {
			telemetryLogger().Warn("Failed to force flush trace provider", "error", err)
			errs = append(errs, err)
		}
	}

	if meterProvider != nil {
		telemetryLogger().Debug("Force flushing meter provider")
		if err := meterProvider.ForceFlush(ctx); err != nil {
			telemetryLogger().Warn("Failed to force flush meter provider", "error", err)
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}
