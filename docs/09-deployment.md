# MacQuiz v2 - Deployment Plan (Low-Cost / Free Tier)

Source: DEP-001 v1.0 (deployment-plan.html), companion to SDD-001 v2.0.
Status: implementation baseline.
Target: $0/month, fallback ~$5/month, no feature cut from v2.

## 1. Hosting constraints imposed by the architecture

Four properties disqualify most free hosting before price is discussed:

| Requirement | Why | What it rules out |
|-------------|-----|-------------------|
| Long-lived WebSockets | Live dashboard, attempt channel, kick delivery < 1 s | Pure serverless; free tiers capping connection duration |
| Always-on scheduler | Quiz open/close jobs and deadline timers fire on server time | Free plans that sleep on idle (a sleeping scheduler misses starts_at) |
| No cold starts during a live window | 1,000+ students start in the same minute | Scale-to-zero platforms; anything needing 30-60 s to wake |
| Redis pub/sub + job queue | Event fan-out over Redis; River jobs in Postgres | HTTP-only serverless Redis with tight command caps |

The one thing v2 does not demand is horizontal scale on day one (~200 DB writes/s, ~250 events/s peak).

## 2. Strategy: one VM, everything on it

The whole backend (API monolith + realtime gateway, workers, PostgreSQL, Redis, reverse proxy) runs as one Docker Compose stack on a single always-free VM.
Managed free services are used only where genuinely better: DNS/CDN, object storage, email, CI.

- Primary host: Oracle Cloud Always Free ARM VM (up to 4 Ampere OCPUs, 24 GB RAM, 200 GB block storage, 10 TB/month egress; permanently free).
- Fallback host: Hetzner CX22 (2 vCPU / 4 GB, ~EUR 3.8 = ~INR 390/month) if Oracle capacity (notably Mumbai) cannot be obtained by launch week.
  The stack is byte-identical either way.
- Colocated Postgres and Redis: removes network latency from the autosave path (helps the < 300 ms p95 target), removes two external dependencies and two bills.
  The trade: backups become our job (see 10-operations.md).
- Cloudflare free in front: DNS, edge TLS, CDN for the static frontend, WebSocket proxying, absorption of casual abuse.

## 3. Topology

| Piece | Deployed as | Cost | Limit that matters |
|-------|-------------|------|--------------------|
| Frontend (React) | Static build on Cloudflare Pages | $0 | 500 builds/month, far above need |
| API + modules | `app` container (Go static binary) | $0 | VM RAM/CPU only |
| Realtime gateway | Same `app` process, `/ws` upgrade path | $0 | ~2k sockets = tens of MB in Go |
| Import/grading workers | `worker` container (same Go binary, worker mode) | $0 | Shares VM cores; queue absorbs bursts |
| PostgreSQL | `postgres:16` container + named volume | $0 | Backups are our responsibility |
| Redis | `redis:7` container, AOF on | $0 | None at this scale |
| Object storage | Cloudflare R2 (S3-compatible) | $0 up to 10 GB | Zero egress fees (the reason to pick R2) |
| Scheduled open/close | River scheduled jobs in worker | $0 | Needs the always-on VM |
| Email | Brevo free (300/day) or Resend (3k/month) | $0 | Credential mail is low-volume |
| Observability | Grafana Cloud free + UptimeRobot | $0 | 14-day retention, acceptable |
| DNS + TLS + CDN | Cloudflare free + Caddy (Let's Encrypt at origin) | $0 | - |
| Domain | Registrar of choice | ~$10/yr | Or $0 on a college subdomain (e.g. under rbmi.in) |

## 4. Compose stack

```yaml
services:
  caddy:
    image: caddy:2
    ports: ["80:80", "443:443"]
    volumes: [./Caddyfile:/etc/caddy/Caddyfile, caddy_data:/data]

  app:                            # API monolith + realtime gateway (/ws)
    image: ghcr.io/ORG/macquiz-app:${TAG}
    env_file: .env.production
    depends_on: [postgres, redis]
    restart: unless-stopped
    deploy: { resources: { limits: { memory: 1g } } }   # Go headroom; typical use is tens of MB

  worker:                         # River: scheduler, grading, imports, rollups
    image: ghcr.io/ORG/macquiz-app:${TAG}
    command: /macquiz worker
    env_file: .env.production
    depends_on: [postgres, redis]
    restart: unless-stopped

  postgres:
    image: postgres:16
    volumes: [pg_data:/var/lib/postgresql/data]
    environment: { POSTGRES_DB: macquiz }
    restart: unless-stopped

  redis:
    image: redis:7
    command: redis-server --appendonly yes   # session durability; queue lives in Postgres
    volumes: [redis_data:/data]
    restart: unless-stopped

volumes: { pg_data: {}, redis_data: {}, caddy_data: {} }
```

Notes:

- API and realtime gateway share one process at this tier; splitting later is a Compose edit.
- The job queue (River) lives in Postgres, so delayed jobs (open/close, deadline timers) survive restarts and even a full Redis loss.
  Belt and braces: the worker also re-scans Postgres for due-but-unfired transitions at boot (the lazy state validation the API already requires).
  Redis keeps AOF on for session durability.
- Firewall: only 80/443 open (Cloudflare IPs only), SSH on a non-standard port with key-only auth; Postgres and Redis never exposed.

## 5. CI/CD (GitHub Actions free minutes)

1. On pull request: lint, typecheck, unit + integration tests (Postgres/Redis as Actions services).
2. On merge to main: build the ARM64 image (Go cross-compiles with `GOARCH=arm64`; the final image is distroless or scratch, ~15-20 MB), push to GHCR, SSH to the VM, `TAG=sha docker compose pull app worker && docker compose up -d app worker`.
3. Migrations run in the app's entrypoint before it accepts traffic.
   Deploys are refused by a pre-deploy check while any quiz is `live` (a self-imposed deploy window).
4. Frontend: Cloudflare Pages builds from the same repo on push; free preview deployments per PR.

## 6. Rejected: the managed-split path

App on Render/Railway, Postgres on Neon/Supabase, Redis on Upstash was evaluated and rejected:

- Render free sleeps after 15 min idle: a sleeping instance misses open/close jobs and cold-starts into the go-live herd.
- Neon free suspends compute: first autosave after wake eats a multi-second cold start against a < 300 ms p95 target, plus WAN latency on every write.
- Upstash free (500k commands/month): one live quiz burns the budget in days.
- Pusher/Ably free (100-200 connections): one 500-student quiz exceeds the cap.

The pattern: managed free tiers are shaped for mostly-idle, lightly-connected apps.
MacQuiz is the opposite: idle for days, then intensely busy and massively connected for one hour.
A VM does not care about that shape.

## 7. Cost tiers and paying triggers

| Tier | Compute | Monthly |
|------|---------|---------|
| Tier 0 (target) | Oracle Always Free ARM VM | $0 (+ ~$10/yr domain, or $0 on a college subdomain) |
| Tier 1 (fallback) | Hetzner CX22 | ~$5 / INR 390 |
| Tier 2 (comfort) | Hetzner CX32 + warm standby with streaming replica + paid backup redundancy | ~$15-20 |

Recommendation: provision Tier 0 now (Oracle ARM capacity hunting runs in the background), launch on Tier 1 if the VM is not secured by launch week, move to Tier 2 once real exams depend on the platform.

Triggers to start paying:

| Trigger | Observed as | First paid move |
|---------|-------------|-----------------|
| Real exams with consequences | Downtime would be an emergency | Tier 2: warm standby + replica, status page |
| Sustained > 3-4k concurrent | WS memory or autosave p95 trending up | Split gateway to its own container/VM |
| Storage growth | R2 > 10 GB | R2 paid ($0.015/GB-month, still no egress fees) |
| Backup anxiety | 24 h RPO no longer acceptable | WAL archiving to R2 (pgBackRest) for point-in-time recovery |
| Email volume | > 300 notifications/day | Paid Brevo/Resend rung (~$10-15/mo) or digests to stay free |
| Ops time exceeds bills | VM babysitting costs more than ~$25/mo of attention | Managed Postgres + two app VMs behind a load balancer; the monolith moves unchanged |

## 8. Accepted compromises at $0

Single region, single node, self-managed backups, and a deploy freeze during live windows instead of zero-downtime rollouts.
The free VM itself is over-provisioned roughly 10x for the stated load; the compromises are operational, not capacity.
