# Grafana Cloud dashboard + alert rules

Checked-in definitions for docs/10-operations.md section 2's key series and section 3's
alert thresholds, so "Grafana Cloud dashboards and alert thresholds" (docs/12-implementation-plan.md
line 86) is not just documented prose but something with a concrete, reviewable artifact in the repo.

Grafana Cloud's free tier is a hosted service with no filesystem access, so these cannot be
dropped into a `provisioning/` directory and picked up automatically the way a self-hosted
Grafana would; they are imported once by hand (or via the HTTP API below), which matches this
project's existing "run by hand" convention for `restore-drill.sh`.

## dashboard.json

Standard Grafana dashboard JSON model with the four series named in docs/10 section 2 (autosave
p95, WebSocket connection count, queue lag, violation/kick rates) plus the worker's
`due_transitions` counter. Panels query PromQL against the metric names Grafana Cloud's OTLP
ingestion (Alloy/Mimir) derives from the OTel instruments in `server/internal/telemetry`
(dots to underscores, unit and `_total`/`_bucket` suffixes per the OTel-to-Prometheus naming
convention) - e.g. `macquiz.attempt.autosave.duration` (a `s`-unit histogram) becomes
`macquiz_attempt_autosave_duration_seconds_bucket`.

Import: Grafana Cloud UI -> Dashboards -> New -> Import -> paste this file's JSON (or upload it),
then map the `DS_PROMETHEUS` input to the stack's built-in Prometheus/Mimir datasource.

## alert-rules.json

Grafana unified-alerting provisioning format (`apiVersion: 1`, `groups`/`rules`), covering the three
thresholds in docs/10 section 3 backed by a metric in this Grafana Cloud stack - two from this
app's own OTel export, one from the `alloy` sidecar container's host metrics (see below):

| Signal | Warn | Page |
|--------|------|------|
| Autosave p95 | > 200 ms sustained 5 min | > 300 ms sustained 5 min |
| Queue lag | > 10 s | > 60 s |
| Disk usage on pg volume | > 70% | > 85% |

Import: Grafana Cloud UI -> Alerting -> Alert rules -> Export/Import -> Import -> upload this
file, mapping `DS_PROMETHEUS` the same way as the dashboard. Alternatively, provision via the
HTTP API with a Grafana Cloud service-account token:

```sh
curl -s -X POST "$GRAFANA_URL/api/v1/provisioning/alert-rules" \
  -H "Authorization: Bearer $GRAFANA_SA_TOKEN" \
  -H "Content-Type: application/json" \
  -d @scripts/grafana/alert-rules.json
```

Both rule groups target the `MacQuiz` folder; create it first (or let the API create it) before
importing.

## alloy-disk-metrics.alloy

Disk usage on the pg volume (docs/10 section 3) has no application-emitted OTel metric behind it -
it's host-level, not a request path `server/internal/telemetry` instruments - so it can't be
sourced from the app's own export the way autosave p95 and queue lag are. `docker-compose.prod.yml`'s
`alloy` container closes that gap with a host metrics agent, as this file previously said would be
needed: it mounts the same `pg_data` named volume postgres writes into (read-only, at `/pgdata`),
runs Alloy's built-in `prometheus.exporter.unix` node_exporter integration scoped to the
`filesystem` collector, and bridges the result through `otelcol.receiver.prometheus` to
`otelcol.exporter.otlphttp`, targeting the exact same Grafana Cloud OTLP gateway
(`MACQUIZ_OTEL_EXPORTER_ENDPOINT`) the app/worker already export to - so `node_filesystem_avail_bytes`/
`node_filesystem_size_bytes` land in the same Grafana Cloud stack and Prometheus/Mimir datasource
`dashboard.json`/`alert-rules.json` already use, queryable by `mountpoint="/pgdata"`. It needs its
own `MACQUIZ_ALLOY_OTLP_AUTH_HEADER` var (see `.env.production.example`) because Alloy's config
sets the header name and value as separate fields rather than the combined `name=value` string
`MACQUIZ_OTEL_EXPORTER_HEADERS` uses - same credential, different shape.

node_exporter's mount-point filter is a Go RE2 regex with no negative-lookahead support, so there
is no clean way to scope the collector to only `/pgdata`; it reports every non-pseudo filesystem it
finds (default excludes already drop `/proc`, `/sys`, `/dev`, and the overlay/docker internals) and
the alert rule/any dashboard query selects `/pgdata` via its `mountpoint` label, the same way an
unwanted series from any other exporter would be filtered.

### Not covered here

A missing/failed backup job is the other docs/10 section 3 threshold with no application-emitted
metric behind it (it's a cron script, not a request path). It's covered a different way:
`scripts/backup/backup.sh` pings an optional `BACKUP_HEALTHCHECK_URL` (a Healthchecks.io-style
dead-man's-switch) on start/success/failure, so a missing or failed nightly dump still pages
someone - just via that external service's own missed-check alerting, not a Grafana rule.
`/healthz` failures (the third row of that table) are already covered by UptimeRobot per docs/10
section 2, not Grafana.

These JSON/Alloy files have not been live-imported/run against a real Grafana Cloud stack (this
environment has none provisioned, so nothing here has a real `MACQUIZ_OTEL_EXPORTER_ENDPOINT` to
export to); the two JSON files are hand-verified as syntactically valid JSON against Grafana's
documented dashboard and alerting-provisioning schemas, and `alloy-disk-metrics.alloy` has been
validated with the real `alloy validate`/`alloy fmt` commands and actually run (`alloy run`) via
Docker against a live `pg_data` volume mounted the same way `docker-compose.prod.yml` mounts it,
confirming the full scrape -> OTLP-bridge pipeline reports `healthy` and produces
`node_filesystem_{avail,size}_bytes{mountpoint="/pgdata"}` series - short of a real Grafana Cloud
account to export the last hop to, this is as close to end-to-end verification as this sandbox
allows.
