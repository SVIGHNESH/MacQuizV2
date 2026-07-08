// Package telemetry wires OpenTelemetry metrics export for the "Monitoring
// on $0" plan (docs/10-operations.md section 2): Grafana Cloud's free OTLP
// endpoint receives the four key series - autosave p95, WebSocket connection
// count, queue lag, and violation/kick event rates. With no endpoint
// configured (dev/test default), every instrument is backed by the OTel
// no-op provider, so nothing needs a local collector running.
package telemetry

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"

	"macquiz/server/internal/config"
)

// Metrics holds the docs/10 section 2 key-series instruments. A nil *Metrics
// answers every method as a no-op, so a module that never had SetMetrics
// called on it (every existing test, and any mode that skips telemetry)
// keeps working unmodified.
type Metrics struct {
	autosaveDuration metric.Float64Histogram
	wsConnections    metric.Int64UpDownCounter
	violationEvents  metric.Int64Counter
	kickEvents       metric.Int64Counter
}

// RecordAutosave records one autosave request's handling latency.
func (m *Metrics) RecordAutosave(ctx context.Context, d time.Duration) {
	if m == nil {
		return
	}
	m.autosaveDuration.Record(ctx, d.Seconds())
}

// IncWSConnections marks one monitor WebSocket as opened.
func (m *Metrics) IncWSConnections(ctx context.Context) {
	if m == nil {
		return
	}
	m.wsConnections.Add(ctx, 1)
}

// DecWSConnections marks one monitor WebSocket as closed.
func (m *Metrics) DecWSConnections(ctx context.Context) {
	if m == nil {
		return
	}
	m.wsConnections.Add(ctx, -1)
}

// RecordViolation counts one guardrail violation report, labeled by type
// (fullscreen, focus, clipboard).
func (m *Metrics) RecordViolation(ctx context.Context, kind string) {
	if m == nil {
		return
	}
	m.violationEvents.Add(ctx, 1, metric.WithAttributes(attribute.String("type", kind)))
}

// RecordKick counts one teacher/admin kick of a live attempt.
func (m *Metrics) RecordKick(ctx context.Context) {
	if m == nil {
		return
	}
	m.kickEvents.Add(ctx, 1)
}

// Provider owns the MeterProvider lifetime and the shared Metrics instance.
type Provider struct {
	Metrics *Metrics

	meter    metric.Meter
	shutdown func(context.Context) error
}

// Shutdown flushes any buffered metrics and stops the exporter. Safe to call
// on a nil Provider or when telemetry was never enabled.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

// Setup builds the MeterProvider named serviceName. With cfg.OTelExporterEndpoint
// unset (the dev/test default), every instrument is a no-op and Setup never
// dials out; otherwise it exports to that OTLP/HTTP endpoint (e.g. Grafana
// Cloud's OTLP gateway) on a periodic 15s interval.
func Setup(ctx context.Context, cfg config.Config, serviceName string) (*Provider, error) {
	if cfg.OTelExporterEndpoint == "" {
		meter := noop.NewMeterProvider().Meter(serviceName)
		metrics, err := newMetrics(meter)
		if err != nil {
			return nil, err
		}
		return &Provider{Metrics: metrics, meter: meter}, nil
	}

	opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(cfg.OTelExporterEndpoint)}
	if headers := parseHeaders(cfg.OTelExporterHeaders); len(headers) > 0 {
		opts = append(opts, otlpmetrichttp.WithHeaders(headers))
	}
	exporter, err := otlpmetrichttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP metric exporter: %w", err)
	}
	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless(attribute.String("service.name", serviceName)))
	if err != nil {
		return nil, fmt.Errorf("build OTel resource: %w", err)
	}
	reader := sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(15*time.Second))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(res))

	meter := mp.Meter("macquiz")
	metrics, err := newMetrics(meter)
	if err != nil {
		return nil, err
	}
	return &Provider{Metrics: metrics, meter: meter, shutdown: mp.Shutdown}, nil
}

func newMetrics(meter metric.Meter) (*Metrics, error) {
	autosave, err := meter.Float64Histogram("macquiz.attempt.autosave.duration",
		metric.WithDescription("Autosave request handling latency"),
		metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	wsConn, err := meter.Int64UpDownCounter("macquiz.realtime.ws_connections",
		metric.WithDescription("Open monitor WebSocket connections"))
	if err != nil {
		return nil, err
	}
	violations, err := meter.Int64Counter("macquiz.attempt.violations",
		metric.WithDescription("Guardrail violation reports, by type"))
	if err != nil {
		return nil, err
	}
	kicks, err := meter.Int64Counter("macquiz.attempt.kicks",
		metric.WithDescription("Teacher/admin kicks of a live attempt"))
	if err != nil {
		return nil, err
	}
	return &Metrics{
		autosaveDuration: autosave,
		wsConnections:    wsConn,
		violationEvents:  violations,
		kickEvents:       kicks,
	}, nil
}

// RegisterQueueLagGauge wires the queue-lag observable gauge (docs/10
// sections 2-3), backed by the same query GET /healthz already uses. poll
// runs on each export interval, never on a request's hot path.
func (p *Provider) RegisterQueueLagGauge(poll func(ctx context.Context) (float64, error)) error {
	if p == nil {
		return nil
	}
	_, err := p.meter.Float64ObservableGauge("macquiz.queue.lag_seconds",
		metric.WithDescription("Seconds the oldest due-but-unfired River job is overdue"),
		metric.WithUnit("s"),
		metric.WithFloat64Callback(func(ctx context.Context, o metric.Float64Observer) error {
			v, err := poll(ctx)
			if err != nil {
				return err
			}
			o.Observe(v)
			return nil
		}),
	)
	return err
}

// parseHeaders parses "key=value,key2=value2" (e.g. Grafana Cloud's
// "Authorization=Basic <token>") into the map otlpmetrichttp.WithHeaders wants.
func parseHeaders(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || k == "" {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}
