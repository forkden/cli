# Forkden CLI

Open-source command-line client for Forkden accounts and private data-plane copy jobs. Licensed under Apache-2.0.

**Release:** `v0.6.0` adds `forkden review`: rehearse a data change on two local copies of your PostgreSQL database and review every row it changed, with no server and no account (see below). `v0.5.0` added browser-approved account login/logout, workspace selection, metadata profiles, clone/fork creation jobs, status/wait/cancel commands and fork-bound work sessions (`cloud session/schema/query/check/history/review/export`) with readable terminal output. The CLI builds independently and contains no engine implementation or account service; since `v0.6.0` it links the PostgreSQL driver `pgx` for the local `review` commands only. `v0.2.0` was the local Unix-socket prototype. Cloud lifecycle adds fork close, clone delete, immutable refresh/version selection and running copy cancellation. The private worker package and `forkden-mcp` ship as a separate signed private release; hosted deployment remains pending.

**Product direction:** one small Apache-2.0 CLI calls the account API for both **self-hosted** customer data planes and **managed cloud** Forkden data planes (a controlled beta today). Private DB execution stays in the selected infrastructure. The `cloud` prefix explicitly uses the account API; existing local commands retain their Unix-socket development transport. [Deployment architecture](https://github.com/forkden/prd/blob/main/docs/08-deployment-architecture.md).

## Account and copy jobs

Build this working tree with `make build` and use `./bin/forkden`. Start with `./bin/forkden --api_url http://127.0.0.1:8080 login` against the configured local backend/portal. Enter the terminal code at `/device`, sign in with GitHub and review/approve the CLI's access. Tokens stay in the OS keyring; settings store the pinned origin and opaque references only. `logout` revokes the token. See [the cloud workflow and contract](docs/cloud-client.md) for workspace selection, clone/fork creation, polling, retries, limits and deployment prerequisites.

## Install or build

Download `forkden_VERSION_OS_ARCH.tar.gz` for macOS or Linux (amd64/arm64) from [GitHub Releases](https://github.com/forkden/cli/releases), then check its checksum and build provenance:

```sh
shasum -a 256 --ignore-missing -c SHA256SUMS
gh attestation verify forkden_0.6.0_darwin_arm64.tar.gz --repo forkden/cli
tar -xzf forkden_0.6.0_darwin_arm64.tar.gz && ./forkden_0.6.0_darwin_arm64/forkden version
```

Or, with Go 1.26 or newer:

```sh
go install github.com/forkden/cli/cmd/forkden@v0.6.0
```

Or clone this repository and run `make build`. Help and version work without an engine or database. No private repository access is needed to build, test or install the CLI. [`forkden-private-release.pub`](forkden-private-release.pub) is the public key for verifying Forkden's private worker/MCP releases.

## Review a data change locally

`forkden review` copies a PostgreSQL database into two databases on a server you choose: `NAME_base` (read-only) and `NAME_work` (where you or your agent make the change). `diff` then compares them row by row and writes a report on your machine. The source is read through one snapshot and never written. Nothing goes to Forkden's cloud, and nothing is started for you: bring a target server (a local Docker container is fine).

`forkden review` is unrelated to `forkden cloud review`, which reviews a fork session on a Forkden worker.

### Prerequisites

- PostgreSQL client tools on `PATH` (`pg_dump`, `pg_restore`) whose **major version equals the target server's**, and a target at least as new as the source (source: PostgreSQL 12 or later). macOS: `brew install postgresql@17` and add `$(brew --prefix postgresql@17)/bin` to `PATH`; Debian/Ubuntu: `apt install postgresql-client-17`.
- A target server where your role may `CREATE DATABASE`. Without `--to`, the copies go on the source server, which then needs that right. For a first run, prefer a separate server: `docker run -d --name review-pg -e POSTGRES_PASSWORD=review -p 5432:5432 postgres:17`.
- A password source: `PGPASSWORD`, `~/.pgpass`, or the DSN. Review never prompts; `pg_dump`/`pg_restore` run with `-w` and receive the password only through `PGPASSWORD`. A password inside a DSN is used but never written to disk, so later commands need `PGPASSWORD` or `~/.pgpass`.

### Quickstart (about 3 minutes with the tools installed)

```sh
export PGPASSWORD=review
TARGET=postgres://postgres@localhost:5432/postgres
psql "$TARGET" -c 'CREATE DATABASE shop'
psql "${TARGET%/*}/shop" <<'SQL'
CREATE TABLE customers (id integer PRIMARY KEY, email text NOT NULL, country text);
CREATE TABLE orders (id bigint PRIMARY KEY, customer_id integer NOT NULL REFERENCES customers (id), status text NOT NULL, total numeric(10, 2) NOT NULL);
CREATE TABLE order_items (order_id bigint REFERENCES orders (id), line integer, sku text NOT NULL, qty integer NOT NULL, PRIMARY KEY (order_id, line));
INSERT INTO customers VALUES (1, 'ada@example.com', 'GB'), (2, 'linus@example.com', NULL), (3, 'grace@example.com', NULL);
INSERT INTO orders VALUES (1, 1, 'paid', 40.00), (2, 2, 'pending', 12.50), (3, 3, 'pending', 7.25);
INSERT INTO order_items VALUES (1, 1, 'BOOK-1', 2), (2, 1, 'PEN-3', 5), (3, 1, 'MUG-2', 1);
SQL

forkden review fork demo --dsn "${TARGET%/*}/shop" --to "$TARGET"
psql "${TARGET%/*}/demo_work" -c "UPDATE customers SET country = 'TR' WHERE country IS NULL"
forkden review diff demo          # ./forkden-review/demo/report.md lists both rows: country null → "TR"
forkden review drop demo
```

`scripts/review-rehearsal.sh` runs the same flow end to end on a throwaway Docker `postgres:17` and prints the elapsed time.

### Commands

| Command | What it does |
|---|---|
| `fork NAME --dsn SOURCE [--to TARGET] [--force] [--template]` | Exports one snapshot of the source, dumps it with `pg_dump`, restores `NAME_base`, fingerprints it, creates `NAME_work` from it, then makes `NAME_base` read-only. Prints the `--to` value and the agent's connection string, and writes `./forkden-review/NAME/target` (password-free) so later commands need no `--to`. `--force` drops an earlier review of the same name first. `--template` copies with `CREATE DATABASE … TEMPLATE` instead (same server, no other sessions on the source). |
| `diff NAME [--to TARGET]` | Compares `NAME_work` to `NAME_base`, each read in one `REPEATABLE READ` snapshot, so writes committed after `diff` starts are not included. Writes `report.md` (for the approver), `report.json` (schema 1) and `changes.sql` (advisory SQL that reproduces the change on a copy of base; not foreign-key ordered, never for production as-is). |
| `diff NAME --base DSN --work DSN` | Compares any two databases you name; `NAME` only names the report directory. |
| `check NAME --file FILE [--to TARGET \| --work DSN]` | Runs boolean SQL checks on `NAME_work` in one read-only snapshot and writes `checks.json`; `report.md` shows the results and whether they saw the state the report compares. |
| `export NAME --output DIR` | Copies the latest complete report into a new directory. |
| `drop NAME [--to TARGET]` | Drops `NAME_base` and `NAME_work`, but only databases that carry fork's marker comment. Reports stay. |

Target resolution: `--to`, then the `target` file written by `fork`, then the libpq variables (`PGHOST`, `PGPORT`, `PGUSER`, `PGDATABASE`). `drop` requires `--to` or the `target` file. Reports go to `./forkden-review/NAME/` (directory `0700`, files `0600`); set `FORKDEN_REVIEW_DIR` to move them. They hold row values from your database: keep them out of synced or shared folders and out of repositories.

A check file holds one boolean query per block; blocks are separated by a blank line, a trailing `;` is ignored, and `-- name:` labels a block:

```sql
-- name: every customer has a country
SELECT count(*) = 0 FROM customers WHERE country IS NULL

-- name: no order lost its items
SELECT NOT EXISTS (SELECT FROM orders o WHERE NOT EXISTS (SELECT FROM order_items i WHERE i.order_id = o.id))
```

### What is and is not compared

Rows are matched by primary key in all non-system schemas. A table is listed under *Not compared* in `report.md`, and the report is **partial** (exit 4), when it has no primary key (`no_primary_key`), its key type has no defined ordering here (`pk_type`; supported: integers, numeric, text/varchar/char, uuid, bytea, boolean, date, timestamp, timestamptz), its definition differs between base and work (`schema_changed`), or it is over 256 MiB or 1,000,000 rows (`too_large`; such tables are copied schema-only). Changes to functions, views, types and other objects are listed separately. If `NAME_base` changed after `fork` (detected by a content fingerprint), the report says so and is partial. `fork` refuses sources with foreign tables or servers, event triggers, `postgres_fdw`/`dblink`/`file_fdw`/`pg_net`/`http`, or untrusted procedural languages, because a copy of them could reach other systems; it also refuses a copy expected to need more than 4 GiB of disk. `pg_dump` takes `ACCESS SHARE` locks for its duration, which block DDL (not reads or writes) on the source: run it at a quiet time.

### Errors

Every error message has the same lines: what happened, `fix:`, `recovery:` (what was or was not changed), `context:`, and for unexpected failures `trace:` with a log under `./forkden-review/NAME/`.

| Code | Exit | Meaning |
|---|---|---|
| `invalid_input` | 2 | A flag, name, file or precondition is wrong; nothing was changed. |
| `unsupported_source` | 2 | The source has objects that reach other systems (`external_reach`); nothing was dumped. |
| `tool_missing` | 1 | `pg_dump`/`pg_restore` are missing or their major version differs from the target server's. |
| `source_unreachable` | 1 | The source cannot be reached or authenticated. |
| `target_unreachable` | 1 | The target server or a copy cannot be reached or authenticated. |
| `source_error` | 1 | The source refused a read (privileges, snapshot export). |
| `dump_failed` | 1 | `pg_dump` failed; nothing was created on the target. |
| `restore_failed` | 1 | `pg_restore` left objects missing (`missing_capture`), or a copy could not be created; partial copies are removed. |
| `freeze_failed` | 1 | Marking, fingerprinting or freezing a copy failed; partial copies are removed. |
| `interrupted` | 130 | Ctrl+C during `fork`; partial copies are removed, or the message names the `drop` command to run. |
| `diff_failed` | 1 | The comparison stopped; no new report was written. |
| `review_partial` | 4 | The report was written, but some tables were not compared or the base changed. |
| `check_failed` | 3 | At least one check returned false or failed; `checks.json` was written. |
| `write_failed` | 1 | A report file could not be written; no half-written report is left. |
| `drop_failed` | 1 | A copy could not be dropped; the message prints the manual `DROP DATABASE` statements. |

## Connect to an engine

The engine serves HTTP API v1 over a local Unix socket. The socket is mode `0600` in the engine's `0700` registry directory. Access is controlled by the local OS user. This transport is separate from cloud account authentication and does not use HTTP proxies or follow redirects. The account HTTP client honors standard HTTP proxy settings and refuses redirects.

```sh
export FORKDEN_ENGINE_SOCKET=/absolute/path/to/engine-state/engine.sock
forkden engine status
```

`--socket PATH` overrides that setting. Otherwise the default is `$FORKDEN_HOME/engine.sock`, or the OS user config directory plus `forkden/engine.sock`.

Source and target URL environment variables must exist in the **engine process**. The CLI sends reference names only and does not resolve source/admin secrets. An external secret manager can inject those variables before starting the engine; provider-specific integrations are not implemented.

## Workflow

```sh
forkden db add sample --url_env FORKDEN_SOURCE_URL
forkden db list
forkden db show sample
forkden db check sample
forkden fork create --source sample --ttl 1h
```

Use the returned `data.id`:

```sh
forkden query --fork fd_REPLACE_WITH_ID --file backfill.sql
forkden check --fork fd_REPLACE_WITH_ID --sql 'SELECT true'
forkden diff --fork fd_REPLACE_WITH_ID
forkden history --fork fd_REPLACE_WITH_ID
forkden export --fork fd_REPLACE_WITH_ID --output ./new-review-directory
forkden fork close fd_REPLACE_WITH_ID
```

Profiles also support `db update NAME --url_env ENV` and `db remove NAME`. `source` is an alias for `db`. The engine verifies the recorded cluster/database and protects profiles with unclosed forks or non-removed clones. `fork prune` closes expired or interrupted forks.

## Reusable clones and versions

With an engine advertising `clones.v1` in `engine status`:

```sh
forkden clone create commerce --source sample
forkden clone show commerce
forkden fork create --clone commerce --version 1 --ttl 1h
forkden clone refresh commerce
forkden fork create --clone commerce --ttl 1h
```

Capture reads the source once into an immutable snapshot on the dedicated target. A fork copies the selected ready version without resolving the source credentials. Omitting `--version` (or using `0`) selects the latest ready version at creation time. Refresh publishes a new version; existing forks keep their original version. A clone pins its target, so `--target_env` is accepted on `clone create`, not on `fork create --clone`. `--source` and `--clone` are mutually exclusive. The original direct `--source` workflow remains supported.

`clone list/show` read lifecycle metadata without a DB connection. Close all dependent forks, including expired ones, before `clone remove NAME_OR_ID`. Removal keeps a metadata tombstone; repeat removal uses its ID. `clone prune` recovers interrupted captures/removals and never deletes ready versions. No automatic janitor or version-retention policy is implemented. After a timeout inspect `clone show`, `fork list` and cleanup state before retrying.

The prototype uses a full PostgreSQL database copy, without copy-on-write or an instant-branching promise. Clone output excludes snapshot rows, internal DB/owner identities, connection references and cluster fingerprints. This local engine protocol does not upload metadata or SQL to a platform.

The engine owns SQL validation, permissions, TTL and audit. The CLI only reads the selected SQL file, sends commands and writes export artifacts locally. Export refuses existing directories and writes files with mode `0600`. SQL and report data can be sensitive even though connection credentials are excluded.

## Output and failures

In a terminal, results use readable summaries and aligned tables. Piped or redirected output remains formatted JSON. `--json` always returns one compact `{ok, data, error?}` envelope; `error` contains a stable `code` and a public `message`. Progress and login instructions go to stderr. Human-readable errors also go to stderr and include a relevant next step. Table lists show at most 20 records and selected columns; use JSON for all records and fields. No colors or terminal animations are emitted into scripts.

Global flags work before or after the command. Conventional names such as `--api-url`, `--config-dir`, `--request-id`, `--url-env` and `--output-dir` are accepted alongside the original underscore spellings. Resource names and flags can appear in either order. Flag values, including SQL, are preserved verbatim; `--` ends option parsing for positional SQL.

```sh
forkden help cloud fork create
forkden cloud session create FORK_RESOURCE_ID --ttl 5m
forkden cloud query --file backfill.sql
forkden cloud job list --format table  # force readable output even when redirected
forkden cloud job list --json         # stable scripting envelope
forkden --version
```

`forkden help COMMAND` and `COMMAND --help` work offline, without opening settings or the keyring. Bash and Zsh completion use the same command catalog and never contact the API:

```sh
source <(forkden completion zsh)   # after compinit, in ~/.zshrc
# or: source <(forkden completion bash)
```

| Exit code | Meaning |
|---|---|
| 0 | Successful command / passed check |
| 1 | Engine, connection, lifecycle or local file error |
| 2 | Invalid input or unsupported protocol/action |
| 3 | Evaluated SQL check returned false |
| 4 | `forkden review diff` wrote a partial report |
| 130 | Command interrupted by Ctrl+C |

An unavailable engine returns an actionable JSON error rather than silently falling back to local database access. The client does not automatically retry commands. A failed or interrupted response may follow a committed operation; inspect `history`/`fork list` before retrying a mutation. `engine_busy` means the command was rejected before execution.

SQL input is limited to 64 KiB. Protocol requests are limited to 256 KiB, responses to 16 MiB. The current engine supports PostgreSQL 17 and a bounded sample-data scope. These limits do not imply suitability for arbitrary production data.

## Development

```sh
make test
make lint
go vet ./...
make build
```

Tests cover the Unix transport, incompatible protocol responses, redirect refusal, cancellation, false-check exit codes and preserving local export paths. CI verifies the repository alone on Linux and macOS.

The `review` database tests need a server and client tools of the same major version; without `FORKDEN_REVIEW_TEST_DSN` they skip with a message:

```sh
FORKDEN_REVIEW_TEST_DSN=postgres://postgres:postgres@localhost:5433/postgres go test ./internal/review/
```

CI runs them in the `verify-db` job against a `postgres:16` service with the runner's PostgreSQL 16 client tools.

### Release checklist

1. `var version` in `cmd/forkden/main.go` and `CHANGELOG.md` name the new version.
2. The commit to tag has a green `verify-db` run; record its URL in the release notes.
3. Tag with the personal signing key (`git tag -s vX.Y.Z`) and push the tag; `release.yml` builds, attests and publishes the archives.
4. On a clean machine: `go install github.com/forkden/cli/cmd/forkden@vX.Y.Z`, `forkden review --help`, then `scripts/review-rehearsal.sh`. [Protocol v1](docs/protocol-v1.md) is public; private engine implementation is outside this repository.

## License

[Apache License 2.0](LICENSE). See [NOTICE](NOTICE). This license applies to the CLI and public protocol/client, not the private engine or backend.
