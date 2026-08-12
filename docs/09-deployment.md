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

> The chosen host is AWS: one EC2 instance in `ap-south-1` running exactly the stack described below.
> Sections 2-7 are the reasoning, which is host-independent; **section 9 is the as-built AWS deployment** and is the one to follow when provisioning.

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
| Frontend (React) | Static build served by `caddy` from the same origin as the API | $0 | Same origin is load-bearing, not a preference - see the Caddyfile header |
| API + modules | `app` container (Go static binary) | $0 | VM RAM/CPU only |
| Realtime gateway | Same `app` process, `/ws` upgrade path | $0 | ~2k sockets = tens of MB in Go |
| Import/grading workers | `worker` container (same Go binary, worker mode) | $0 | Shares VM cores; queue absorbs bursts |
| PostgreSQL | `postgres:16` container + named volume | $0 | Backups are our responsibility |
| Redis | `redis:7` container, AOF on | $0 | None at this scale |
| Object storage | S3 bucket, same region as the instance | ~$0 at a few GB | In-region traffic to EC2 is free, so only stored bytes cost anything |
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

This is checked into the repo as `docker-compose.prod.yml` plus a `Caddyfile` and a `scripts/backup` directory (the nightly pg_dump-to-S3 cron container, 10-operations.md section 1), with `.env.production.example` documenting every required env var.
Initial VM provisioning copies all three to `/opt/macquiz` (compose file as `docker-compose.yml`) and fills in `.env.production` from the example; every later deploy (section 5) only ever runs `docker compose pull app worker && docker compose up -d app worker` against what is already there, so postgres/redis/caddy/backup start once and are left running across deploys.

Notes:

- API and realtime gateway share one process at this tier; splitting later is a Compose edit.
- The job queue (River) lives in Postgres, so delayed jobs (open/close, deadline timers) survive restarts and even a full Redis loss.
  Belt and braces: the worker also re-scans Postgres for due-but-unfired transitions at boot (the lazy state validation the API already requires).
  Redis keeps AOF on for session durability.
- Firewall: only 80/443 and SSH open, key-only auth; Postgres and Redis never exposed. The as-built security group is section 9.3.

## 5. CI/CD (GitHub Actions free minutes)

1. On pull request: lint, typecheck, unit + integration tests (Postgres/Redis as Actions services).
2. On merge to main: build the ARM64 image (Go cross-compiles with `GOARCH=arm64`; the final image is distroless or scratch, ~15-20 MB), push to GHCR, SSH to the VM, `TAG=sha docker compose pull app worker && docker compose up -d app worker`.
3. Migrations run in the app's entrypoint before it accepts traffic.
   Deploys are refused by a pre-deploy check while any quiz is `live` (a self-imposed deploy window).
4. Frontend: the same workflow builds the SPA and uploads `web/dist` to the host, so it ships on the same origin and in the same deploy as the API it talks to.

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
| Storage growth | S3 bucket growth on the import/avatar bucket | S3 is already pay-per-GB; move cold imports to a lifecycle rule into Infrequent Access |
| Backup anxiety | 24 h RPO no longer acceptable | WAL archiving to S3 (pgBackRest) for point-in-time recovery |
| Email volume | > 300 notifications/day | Paid Brevo/Resend rung (~$10-15/mo) or digests to stay free |
| Ops time exceeds bills | VM babysitting costs more than ~$25/mo of attention | Managed Postgres + two app VMs behind a load balancer; the monolith moves unchanged |

## 8. Accepted compromises at $0

Single region, single node, self-managed backups, and a deploy freeze during live windows instead of zero-downtime rollouts.
The free VM itself is over-provisioned roughly 10x for the stated load; the compromises are operational, not capacity.

## 9. AWS deployment (as built)

Section 2's argument - one always-on box, everything colocated - is what AWS gets asked for here.
The managed-split alternative (ECS Fargate + RDS + ElastiCache + ALB) was reconsidered at the AWS port and rejected on the same grounds section 6 rejected the Render/Neon/Upstash split, plus two AWS-specific ones: it costs roughly ten times as much at this load, and it dissolves the loopback `/deploy-check` guard that implements invariant 7.

### 9.1 What runs where

| Piece | AWS resource | Notes |
|-------|--------------|-------|
| The whole stack (caddy, app, worker, postgres, redis, backup, alloy) | One EC2 instance, `ap-south-1` | `t4g.small` (2 vCPU Graviton, 2 GB) to start; `t4g.medium` if a live window ever shows memory pressure |
| Public address | Elastic IP | Static across stop/start, which a plain public IP is not - the DNS record and the TLS certificate both depend on it |
| Disk | 30 GB gp3 root volume | Holds the Docker volumes (`pg_data`, `redis_data`, `caddy_data`, `import_data`); gp3's baseline 3000 IOPS is well clear of the ~200 writes/s peak |
| Import files + avatar photos | S3 bucket `macquiz-blobs` | One bucket, avatars under an `avatars/` key prefix |
| Nightly dumps | S3 bucket `macquiz-backups`, versioning on | Separate bucket so an app-side bug cannot reach the dumps |
| Credentials for both buckets | IAM instance profile `MacQuizHostRole` | `scripts/aws/iam-policy.json`; no long-lived key pair on the box |
| TLS | Caddy + Let's Encrypt, on the instance | Not ACM: ACM certificates only attach to an ALB/CloudFront, and adding one to terminate TLS would cost more per month than the instance |
| Images | GHCR | Not ECR - it would add OIDC role assumption to the workflow and buy nothing at one image per deploy |

Everything above except the two buckets and the IAM role is a single instance, so the whole production footprint is four resources.

### 9.2 Architecture: arm64

The instance is Graviton, so `.github/workflows/deploy.yml` builds `linux/arm64`.
The Dockerfile is `CGO_ENABLED=0` static Go onto distroless, so the x86 GitHub runner cross-compiles it directly - no QEMU, no build-time penalty.
Moving to an x86 instance type is a one-word change back to `linux/amd64`.

### 9.3 Security groups

| Port | Source | Why |
|------|--------|-----|
| 443, 80 | `0.0.0.0/0` | Public traffic; 80 is required for the Let's Encrypt HTTP challenge and Caddy's redirect to HTTPS |
| 22 | Your admin IP, and nothing else | Deploys arrive over SSH from GitHub Actions, whose IP ranges are wide - see the note below |

Postgres (5432), Redis (6379), and the app's own 8080 are never in a security group at all: they are container ports on the Docker bridge, and `docker-compose.prod.yml` binds the app to `127.0.0.1:8080` specifically so it stays off every interface but loopback.

GitHub Actions runners have no stable IP range worth allowlisting.
Two workable answers: leave 22 open to `0.0.0.0/0` with password auth disabled and key-only access (the default the deploy workflow assumes), or put the instance behind EC2 Instance Connect Endpoint / SSM Session Manager and close 22 entirely, which then requires reworking the three `appleboy/*` steps in the workflow.
Start with the former; the latter is the hardening step to take once real exams depend on the platform.

### 9.4 IMDSv2 hop limit

The app, worker, and backup containers get their AWS credentials from the instance role over IMDS.
A request from inside a container crosses one extra network hop (the Docker bridge), and EC2's default `http-put-response-hop-limit` of 1 silently drops it - every S3 call then fails with "no EC2 IMDS role found" while the instance itself can reach IMDS fine.

```sh
aws ec2 modify-instance-metadata-options --region ap-south-1 \
  --instance-id <id> --http-tokens required --http-put-response-hop-limit 2
```

`scripts/aws/provision.sh` checks for the role and prints this command; it cannot set the limit itself, since that is an API call from outside the instance.

### 9.5 First deploy, start to finish

1. Create the two S3 buckets in `ap-south-1`, block all public access on both, and turn on versioning for `macquiz-backups`.
2. Create IAM role `MacQuizHostRole` (trusted entity: EC2) with `scripts/aws/iam-policy.json` as its permission policy, with the real bucket names substituted in.
3. Launch a `t4g.small` running Ubuntu 24.04 LTS (arm64) with a 30 GB gp3 root volume, the security group from 9.3, and `MacQuizHostRole` as its instance profile. Allocate an Elastic IP and associate it.
4. Raise the IMDSv2 hop limit (9.4).
5. Point `DOMAIN`'s A record at the Elastic IP. Do this before step 7: Caddy requests its certificate on first start and the HTTP challenge needs the record to already resolve.
6. SSH in and provision:
   ```sh
   git clone https://github.com/<org>/MacQuizV2 /tmp/macquiz-src
   sudo /tmp/macquiz-src/scripts/aws/provision.sh /tmp/macquiz-src
   ```
7. Fill in `/opt/macquiz/.env.production` (every `replace-with-...` value, plus `DOMAIN` and `MACQUIZ_IMAGE`), `docker login ghcr.io`, then `cd /opt/macquiz && docker compose up -d`.
8. Seed the first admin, once ever: `docker compose run --rm app bootstrap`.
9. In the GitHub repo, set the `DEPLOY_SSH_HOST` / `DEPLOY_SSH_USER` / `DEPLOY_SSH_KEY` secrets and flip the `DEPLOY_ENABLED` variable to `true`. Every later deploy is then a merge to `main`.

### 9.6 What is actually deployed (first deploy, 12 Aug 2026)

| Resource | Value |
|----------|-------|
| Account / region | 344859352801 / `ap-south-1` |
| Instance | `i-0e300278af8726c1b`, `t4g.small`, Ubuntu 24.04 arm64, 30 GB gp3 (encrypted) |
| Elastic IP | `13.204.107.241` |
| Security group | `sg-081606632073f0e53` - 80, 443, 22 |
| Instance profile | `MacQuizHostProfile` -> `MacQuizHostRole`, IMDSv2 hop limit 2 set at launch |
| Buckets | `macquiz-blobs-344859352801`, `macquiz-backups-344859352801` (versioned); both SSE-S3, all public access blocked |
| SSH key | `macquiz-prod`, private key at `~/.ssh/macquiz-prod.pem` on the operator's workstation |

Three deliberate departures from the target state above, all of them temporary:

- **No TLS and no domain.** `DOMAIN=:80` and `MACQUIZ_ENV=development`, which move together for the reason in the Caddyfile header. Anything sensitive is travelling in clear text, so this is a staging posture, not a posture to run a real exam on.
- **The image is a local build**, tagged `macquiz-app:local`, built once on the instance. GHCR holds nothing yet.
- **CI deploys are still off.** `DEPLOY_ENABLED` is unset and the `DEPLOY_SSH_*` secrets are not configured, so a merge to `main` ships nothing.

To finish the job: point a hostname at the Elastic IP, set `DOMAIN` to it and `MACQUIZ_ENV=production`, restart caddy and app, then set the three GitHub secrets and flip `DEPLOY_ENABLED`.

### 9.7 Cost

| Line | Monthly (ap-south-1, on-demand) |
|------|-------------------------------|
| `t4g.small`, always on | ~$12 |
| 30 GB gp3 | ~$2.50 |
| Elastic IP (attached) | ~$3.60 |
| S3 (a few GB, in-region traffic to EC2 is free) | < $1 |
| **Total** | **~$19** |

A one-year Compute Savings Plan or Reserved Instance takes the compute line down by roughly 40%.
The first 12 months of a new AWS account cover a `t4g.small` under the free tier, so the practical launch cost is the volume and the Elastic IP.

This is above section 7's Tier 0 target of $0 and roughly Tier 1's price.
The trade bought by paying it: `ap-south-1` is a Mumbai region with predictable capacity, which is exactly what section 7 flagged as the risk in the Oracle Always Free plan.
