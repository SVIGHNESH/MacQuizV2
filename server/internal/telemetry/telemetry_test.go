package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"

	"macquiz/server/internal/config"
)

func TestSetupDisabledIsNoop(t *testing.T) {
	ctx := context.Background()
	p, err := Setup(ctx, config.Config{}, "macquiz-test")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// None of these should panic or block: with no OTLP endpoint configured,
	// every instrument is backed by the OTel no-op provider.
	p.Metrics.RecordAutosave(ctx, 10*time.Millisecond)
	p.Metrics.IncWSConnections(ctx)
	p.Metrics.DecWSConnections(ctx)
	p.Metrics.RecordViolation(ctx, "fullscreen")
	p.Metrics.RecordKick(ctx)
	p.Metrics.RecordDueTransitions(ctx, "quizzes_opened", 3)

	if err := p.RegisterQueueLagGauge(func(context.Context) (float64, error) {
		return 0, nil
	}); err != nil {
		t.Fatalf("RegisterQueueLagGauge: %v", err)
	}

	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestNilMetricsIsSafe(t *testing.T) {
	// A Handler/Gateway that never had SetMetrics called on it holds a nil
	// *Metrics; every method must no-op rather than panic.
	var m *Metrics
	ctx := context.Background()
	m.RecordAutosave(ctx, time.Second)
	m.IncWSConnections(ctx)
	m.DecWSConnections(ctx)
	m.RecordViolation(ctx, "focus")
	m.RecordKick(ctx)
	m.RecordDueTransitions(ctx, "quizzes_opened", 1)
}

func TestNilProviderMethodsAreSafe(t *testing.T) {
	var p *Provider
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown on nil Provider: %v", err)
	}
	if err := p.RegisterQueueLagGauge(func(context.Context) (float64, error) {
		return 0, nil
	}); err != nil {
		t.Fatalf("RegisterQueueLagGauge on nil Provider: %v", err)
	}
}

func TestSetupPropagatesQueueLagError(t *testing.T) {
	p, err := Setup(context.Background(), config.Config{}, "macquiz-test")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	wantErr := errors.New("boom")
	if err := p.RegisterQueueLagGauge(func(context.Context) (float64, error) {
		return 0, wantErr
	}); err != nil {
		t.Fatalf("RegisterQueueLagGauge: %v", err)
	}
	// The callback error only surfaces when the reader actually collects; the
	// no-op provider never calls back, so registering it must not itself fail.
}

func TestParseHeaders(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{"empty", "", nil},
		{"blank", "   ", nil},
		{"single", "Authorization=Basic abc123", map[string]string{"Authorization": "Basic abc123"}},
		{"multi", "a=1,b=2", map[string]string{"a": "1", "b": "2"}},
		{"spaced", " a = 1 , b = 2 ", map[string]string{"a": "1", "b": "2"}},
		{"no-equals-skipped", "a=1,noequals,b=2", map[string]string{"a": "1", "b": "2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseHeaders(c.raw)
			if len(got) != len(c.want) {
				t.Fatalf("parseHeaders(%q) = %v, want %v", c.raw, got, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Fatalf("parseHeaders(%q)[%q] = %q, want %q", c.raw, k, got[k], v)
				}
			}
		})
	}
}

func TestSetupEnabledBuildsExporter(t *testing.T) {
	// otlpmetrichttp.New only dials lazily on export, so Setup must succeed
	// even against an unreachable endpoint - this exercises the "enabled"
	// branch (exporter + resource + periodic reader construction) without a
	// real collector.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := Setup(ctx, config.Config{
		OTelExporterEndpoint: "127.0.0.1:1",
		OTelExporterHeaders:  "Authorization=Basic abc123",
	}, "macquiz-test")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	p.Metrics.RecordAutosave(ctx, time.Millisecond)
	if err := p.RegisterQueueLagGauge(func(context.Context) (float64, error) { return 0, nil }); err != nil {
		t.Fatalf("RegisterQueueLagGauge: %v", err)
	}
	// Shutdown flushes the recorded point to the (unreachable) endpoint, so it
	// is expected to return a network error here; the assertion is only that
	// it returns rather than hangs or panics.
	_ = p.Shutdown(ctx)
}
