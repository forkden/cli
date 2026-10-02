# Forkden CLI

Open-source command-line client for the private Forkden engine. Licensed under Apache-2.0.

**Status:** local prototype, macOS/Linux, engine protocol v1. The CLI builds independently and contains no engine implementation, PostgreSQL driver, copy logic or account service. A separately running engine is required for database operations. Engine distribution and cloud account login are not available publicly yet.

## Install or build

Requires Go 1.26 or newer:

```sh
go install github.com/forkden/cli/cmd/forkden@v0.2.0
forkden version
forkden help
```

Or clone this repository and run `make build`. Help and version work without an engine or database. No private repository access is needed to build, test or install the CLI.

## Connect to an engine

The engine serves HTTP API v1 over a local Unix socket. The socket is mode `0600` in the engine's `0700` registry directory. Access is controlled by the local OS user; this is not cloud account authentication. The CLI has no TCP/remote endpoint setting and does not use HTTP proxies or follow redirects.

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

Global flags precede commands. Default output is formatted JSON; `--json` returns a compact `{ok, data, error?}` envelope. `error` contains a stable `code` and a public `message`.

| Exit code | Meaning |
|---|---|
| 0 | Successful command / passed check |
| 1 | Engine, connection, lifecycle or local file error |
| 2 | Invalid input or unsupported protocol/action |
| 3 | Evaluated SQL check returned false |

An unavailable engine returns an actionable JSON error rather than silently falling back to local database access. The client does not automatically retry commands. A failed or interrupted response may follow a committed operation; inspect `history`/`fork list` before retrying a mutation. `engine_busy` means the command was rejected before execution.

SQL input is limited to 64 KiB. Protocol requests are limited to 256 KiB, responses to 16 MiB. The current engine supports PostgreSQL 17 and a bounded sample-data scope. These limits do not imply suitability for arbitrary production data.

## Development

```sh
make test
make lint
go vet ./...
make build
```

Tests cover the Unix transport, incompatible protocol responses, redirect refusal, cancellation, false-check exit codes and preserving local export paths. CI verifies the repository alone on Linux and macOS. [Protocol v1](docs/protocol-v1.md) is public; private engine implementation is outside this repository.

## License

[Apache License 2.0](LICENSE). See [NOTICE](NOTICE). This license applies to the CLI and public protocol/client, not the private engine or backend.
