package logger

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
)

const serviceName = "sparks-effect-routing-worker"

func OTLP(ctx context.Context, enabled bool) (slog.Handler, func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if !enabled {
		return nil, noop, nil
	}

	// On Railway nothing tails the container's stderr into Loki the way Alloy
	// tailed the pod's, so the worker ships its own log records to the same
	// Grafana Cloud gateway the API uses. The exporter reads the standard
	// OTEL_EXPORTER_OTLP_* variables itself (endpoint, headers, timeout), so
	// the credentials Grafana Cloud's OpenTelemetry card hands out are pasted
	// in as they come.
	exporter, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, noop, fmt.Errorf("logger: building the OTLP exporter: %w", err)
	}
	// Later options win, so OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES
	// override the default name. The two worker services are told apart by
	// service.namespace, which Grafana folds into the job label, the same
	// way the API tells its staging from its production.
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", serviceName)),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, noop, fmt.Errorf("logger: building the resource: %w", err)
	}

	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
		sdklog.WithResource(res),
	)
	return otelslog.NewHandler(serviceName, otelslog.WithLoggerProvider(provider)), provider.Shutdown, nil
}
