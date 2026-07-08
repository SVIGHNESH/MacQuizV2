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

Grafana unified-alerting provisioning format (`apiVersion: 1`, `groups`/`rules`), covering the two
thresholds in docs/10 section 3 that are derived from metrics this app actually emits:

| Signal | Warn | Page |
|--------|------|------|
| Autosave p95 | > 200 ms sustained 5 min | > 300 ms sustained 5 min |
| Queue lag | > 10 s | > 60 s |

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

### Not covered here

docs/10 section 3 also lists disk usage on the pg volume and a missing/failed backup job. Neither
has an application-emitted OTel metric behind it (disk usage is host-level, and the backup job is
a cron script, not a request path `server/internal/telemetry` instruments) - wiring those up would
mean a host metrics agent (e.g. Grafana Alloy's node exporter integration) and a dead-man's-switch
ping from `scripts/backup/backup.sh` (e.g. to Healthchecks.io), which is separate infrastructure
work, not a dashboard/alert-rule authoring gap. `/healthz` failures (the third row of that table)
are already covered by UptimeRobot per docs/10 section 2, not Grafana.

These two JSON files have not been live-imported against a real Grafana Cloud stack (this
environment has none provisioned); they are hand-verified as syntactically valid JSON against
Grafana's documented dashboard and alerting-provisioning schemas, matching the metric names and
thresholds that actually exist in this codebase (`server/internal/telemetry/telemetry.go`,
docs/10-operations.md section 3) rather than live-tested end to end.
