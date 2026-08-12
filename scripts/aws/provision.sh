#!/usr/bin/env bash
# One-time provisioning for the MacQuiz production host on EC2
# (docs/09-deployment.md section 9). Run once on a fresh Ubuntu 24.04 LTS
# instance, as the default `ubuntu` user, from a checkout of this repo:
#
#   git clone https://github.com/<org>/MacQuizV2 /tmp/macquiz-src
#   sudo /tmp/macquiz-src/scripts/aws/provision.sh /tmp/macquiz-src
#
# It is deliberately NOT the deploy path. Every later deploy is
# .github/workflows/deploy.yml, which only ever uploads the SPA and runs
# `docker compose pull app worker && docker compose up -d app worker`
# against what this script laid down. Re-running it is safe: it never
# touches .env.production once that file exists, and never restarts a
# running stack.
set -euo pipefail

src="${1:-}"
if [ -z "$src" ] || [ ! -f "$src/docker-compose.prod.yml" ]; then
  echo "usage: $0 <path to a MacQuizV2 checkout>" >&2
  exit 1
fi

if [ "$(id -u)" -ne 0 ]; then
  echo "must run as root (sudo)" >&2
  exit 1
fi

target=/opt/macquiz
service_user="${SUDO_USER:-ubuntu}"

echo "==> installing Docker Engine + compose plugin"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq ca-certificates curl gnupg unattended-upgrades
install -m 0755 -d /etc/apt/keyrings
if [ ! -f /etc/apt/keyrings/docker.asc ]; then
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
fi
cat >/etc/apt/sources.list.d/docker.list <<EOF
deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable
EOF
apt-get update -qq
apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
systemctl enable --now docker
usermod -aG docker "$service_user"

# Unattended security upgrades only. The stack itself is pinned by image
# tag and rolled forward by CI, so the host should never reboot itself
# mid-exam; kernel updates wait for a hand-picked window.
echo "==> enabling unattended security upgrades (no automatic reboot)"
cat >/etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF
sed -i 's|^//\?Unattended-Upgrade::Automatic-Reboot .*|Unattended-Upgrade::Automatic-Reboot "false";|' \
  /etc/apt/apt.conf.d/50unattended-upgrades || true

# t4g.small is 2 GB. Postgres + Redis + two Go processes fit, but a large
# import or an analytics rollup can spike; a swap file turns what would be
# an OOM-kill mid-exam into a slow minute.
if [ ! -f /swapfile ]; then
  echo "==> creating a 2G swap file"
  fallocate -l 2G /swapfile
  chmod 600 /swapfile
  mkswap /swapfile >/dev/null
  swapon /swapfile
  echo '/swapfile none swap sw 0 0' >>/etc/fstab
fi
# Redis logs a warning without this and a background save can fail under
# memory pressure (see its startup output on the dev stack).
if ! grep -q '^vm.overcommit_memory' /etc/sysctl.d/99-macquiz.conf 2>/dev/null; then
  echo "==> setting vm.overcommit_memory=1 for Redis"
  echo 'vm.overcommit_memory = 1' >>/etc/sysctl.d/99-macquiz.conf
  sysctl -p /etc/sysctl.d/99-macquiz.conf >/dev/null
fi

echo "==> laying out ${target}"
mkdir -p "$target/www"
# docker-compose.prod.yml becomes the host's docker-compose.yml, because
# deploy.yml runs a bare `docker compose` from this directory.
install -m 0644 "$src/docker-compose.prod.yml" "$target/docker-compose.yml"
install -m 0644 "$src/Caddyfile" "$target/Caddyfile"
rm -rf "$target/scripts"
mkdir -p "$target/scripts"
cp -r "$src/scripts/backup" "$target/scripts/backup"
cp -r "$src/scripts/grafana" "$target/scripts/grafana"
chown -R "$service_user":"$service_user" "$target"

if [ ! -f "$target/.env.production" ]; then
  install -m 0600 -o "$service_user" -g "$service_user" \
    "$src/.env.production.example" "$target/.env.production"
  echo
  echo "==> ${target}/.env.production was seeded from the example."
  echo "    Fill in every 'replace-with-...' value before starting the stack."
else
  echo "==> ${target}/.env.production already exists, left untouched"
fi

# The app, worker, and backup containers read AWS credentials from the
# instance role over IMDSv2. A container's request crosses one extra network
# hop (the docker bridge), and EC2's default response hop limit of 1 drops
# it - so every AWS call inside a container fails with "no EC2 IMDS role
# found" until the limit is raised. This is the single most common way the
# S3 legs of this stack break, so assert it here rather than at 2am.
echo "==> checking the IMDSv2 hop limit"
token=$(curl -fsS -X PUT "http://169.254.169.254/latest/api/token" \
  -H "X-aws-ec2-metadata-token-ttl-seconds: 60" 2>/dev/null || true)
if [ -z "$token" ]; then
  echo "    WARNING: IMDS unreachable from the host; skipping the check." >&2
elif ! curl -fsS -H "X-aws-ec2-metadata-token: $token" \
  http://169.254.169.254/latest/meta-data/iam/security-credentials/ >/dev/null 2>&1; then
  echo "    WARNING: this instance has no IAM role attached. Attach the" >&2
  echo "    MacQuizHostRole (scripts/aws/iam-policy.json) or the S3 legs" >&2
  echo "    (imports, avatars, nightly backups) will not work." >&2
else
  echo "    IAM role present. Confirm the hop limit is 2 from your workstation:"
  echo "      aws ec2 modify-instance-metadata-options --region ap-south-1 \\"
  echo "        --instance-id <id> --http-tokens required --http-put-response-hop-limit 2"
fi

cat <<EOF

Provisioning done. Remaining manual steps (docs/09-deployment.md section 9.5):

  1. Edit ${target}/.env.production - every secret, DOMAIN, MACQUIZ_IMAGE.
  2. Log in to GHCR so 'docker compose pull' can read the private image:
       echo <github-pat-with-read:packages> | docker login ghcr.io -u <user> --password-stdin
  3. Point DOMAIN's A record at this instance's Elastic IP, then:
       cd ${target} && docker compose up -d
     Caddy gets its Let's Encrypt certificate on that first start, which
     needs the A record to already resolve here.
  4. Seed the first admin (once, ever):
       cd ${target} && docker compose run --rm app bootstrap
  5. In the GitHub repo: set the DEPLOY_SSH_HOST / DEPLOY_SSH_USER /
     DEPLOY_SSH_KEY secrets and flip the DEPLOY_ENABLED variable to "true".

You have been added to the docker group - log out and back in for it to
take effect in your shell.
EOF
