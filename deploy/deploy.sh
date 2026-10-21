#!/usr/bin/env bash
# Update the running stack to the latest main and rebuild it.
# Run on the server (from anywhere):  ~/maskinsjekk/deploy/deploy.sh
# or remotely:                        ssh <server> '~/maskinsjekk/deploy/deploy.sh'
#
# The SQLite database lives in a named Docker volume, so redeploying never
# touches stored data.
set -euo pipefail

cd "$(dirname "$0")"

before=$(git rev-parse HEAD)
git pull --ff-only --quiet
after=$(git rev-parse HEAD)

if [ "$before" = "$after" ] && [ "${1:-}" != "--force" ]; then
  echo "already at $(git log --oneline -1); nothing to deploy (use --force to rebuild anyway)"
  exit 0
fi

echo "deploying $(git log --oneline -1)"
docker compose up -d --build --remove-orphans

port=${HTTP_PORT:-8080}
for _ in $(seq 1 30); do
  if curl -fsS "http://localhost:$port/healthz" >/dev/null 2>&1; then
    echo "healthy on :$port"
    exit 0
  fi
  sleep 1
done
echo "service did not become healthy; recent logs:" >&2
docker compose logs --tail 50 >&2
exit 1
