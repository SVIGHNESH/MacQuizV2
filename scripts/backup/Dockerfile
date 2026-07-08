# Nightly pg_dump -> R2 cron container (docs/10-operations.md section 1).
# Based on the same postgres:16 major version as docker-compose.prod.yml's
# postgres service so pg_dump's format matches the server it dumps.
FROM postgres:16-alpine

RUN apk add --no-cache aws-cli bash

COPY backup.sh /backup.sh
COPY crontab /etc/crontabs/root
RUN chmod +x /backup.sh

# Foreground, non-daemonizing crond so the container stays up and its exit
# is visible to Compose/restart policies; -l 2 logs job start/end.
ENTRYPOINT ["crond", "-f", "-l", "2"]
