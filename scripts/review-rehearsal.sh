#!/usr/bin/env bash
# Rehearse the forkden review flow end to end on a throwaway Docker postgres:17:
# fixture -> fork -> an agent-style UPDATE -> diff -> check -> export -> drop.
# Needs docker, psql and pg_dump/pg_restore 17 on PATH. Prints the elapsed time and
# the report's timings; exits non-zero if any step fails.
#
#   scripts/review-rehearsal.sh            # uses ./bin/forkden, building it if missing
#   FORKDEN=forkden scripts/review-rehearsal.sh
set -euo pipefail

cd "$(dirname "$0")/.."
FORKDEN=${FORKDEN:-./bin/forkden}
PORT=${REHEARSAL_PORT:-55432}
CONTAINER=forkden-review-rehearsal-$$
WORKDIR=$(mktemp -d)

cleanup() {
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

for tool in docker psql pg_dump pg_restore; do
  command -v "$tool" >/dev/null || { echo "rehearsal: $tool is not on PATH" >&2; exit 2; }
done
pg_dump --version | grep -q '(PostgreSQL) 17\.' || { echo "rehearsal: needs PostgreSQL 17 client tools, found: $(pg_dump --version)" >&2; exit 2; }
if [ "$FORKDEN" = ./bin/forkden ] && [ ! -x ./bin/forkden ]; then
  go build -trimpath -o bin/forkden ./cmd/forkden
fi
FORKDEN=$(cd "$(dirname "$FORKDEN")" && pwd)/$(basename "$FORKDEN")

start=$(date +%s)
docker run -d --name "$CONTAINER" -e POSTGRES_PASSWORD=rehearsal -p "127.0.0.1:$PORT:5432" postgres:17 >/dev/null
export PGPASSWORD=rehearsal
TARGET="postgres://postgres@127.0.0.1:$PORT/postgres"
for _ in $(seq 1 60); do
  psql "$TARGET" -Atqc 'SELECT 1' >/dev/null 2>&1 && break
  sleep 1
done
psql "$TARGET" -qc 'CREATE DATABASE shop'
psql "${TARGET%/*}/shop" -q -v ON_ERROR_STOP=1 -f internal/review/testdata/fixture.sql

cd "$WORKDIR"
"$FORKDEN" review fork demo --dsn "${TARGET%/*}/shop" --to "$TARGET" --json >/dev/null
psql "${TARGET%/*}/demo_work" -qc "UPDATE customers SET country = 'TR' WHERE country IS NULL"
"$FORKDEN" review diff demo --json >/dev/null
grep -q 'country: `null` → `"TR"`' forkden-review/demo/report.md || { echo "rehearsal: report.md lacks the expected change" >&2; exit 1; }
cat > checks.sql <<'SQL'
-- name: every customer has a country
SELECT count(*) = 0 FROM customers WHERE country IS NULL
SQL
"$FORKDEN" review check demo --file checks.sql --json >/dev/null
"$FORKDEN" review export demo --output exported --json >/dev/null
"$FORKDEN" review drop demo --json >/dev/null
elapsed=$(( $(date +%s) - start ))

python3 -c 'import json,sys; t=json.load(open(sys.argv[1]))["timings"]; print("timings:", " ".join(f"{k}={v}" for k, v in t.items()))' exported/report.json
echo "elapsed: ${elapsed}s (container start included)"
echo "rehearsal: ok"
