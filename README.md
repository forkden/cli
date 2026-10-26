# Forkden CLI

Open-source command-line client for Forkden accounts and private data-plane copy jobs. Licensed under Apache-2.0.

**Release:** `v0.5.0` adds browser-approved account login/logout, workspace selection, metadata profiles, clone/fork creation jobs, status/wait/cancel commands and fork-bound work sessions (`cloud session/schema/query/check/history/review/export`) with readable terminal output. The CLI builds independently and contains no engine implementation, PostgreSQL driver, copy logic or account service. `v0.2.0` was the local Unix-socket prototype. Cloud lifecycle adds fork close, clone delete, immutable refresh/version selection and running copy cancellation. The private worker package and `forkden-mcp` ship as a separate signed private release; hosted deployment remains pending.

**Product direction:** one small Apache-2.0 CLI calls the account API for both **self-hosted** customer data planes and future **managed cloud** Forkden data planes. Private DB execution stays in the selected infrastructure. The `cloud` prefix explicitly uses the account API; existing local commands retain their Unix-socket development transport. [Deployment architecture](https://github.com/forkden/prd/blob/main/docs/08-deployment-architecture.md).

## Account and copy jobs

Build this working tree with `make build` and use `./bin/forkden`. Start with `./bin/forkden --api_url http://127.0.0.1:8080 login` against the configured local backend/portal. Enter the terminal code at `/device`, sign in with GitHub and review/approve the CLI's access. Tokens stay in the OS keyring; settings store the pinned origin and opaque references only. `logout` revokes the token. See [the cloud workflow and contract](docs/cloud-client.md) for workspace selection, clone/fork creation, polling, retries, limits and deployment prerequisites.

## Install or build

Download `forkden_VERSION_OS_ARCH.tar.gz` for macOS or Linux (amd64/arm64) from [GitHub Releases](https://github.com/forkden/cli/releases), then check its checksum and build provenance:

```sh
shasum -a 256 --ignore-missing -c SHA256SUMS
gh attestation verify forkden_0.5.0_darwin_arm64.tar.gz --repo forkden/cli
tar -xzf forkden_0.5.0_darwin_arm64.tar.gz && ./forkden_0.5.0_darwin_arm64/forkden version
```

Or, with Go 1.26 or newer:

```sh
go install github.com/forkden/cli/cmd/forkden@v0.5.0
```

Or clone this repository and run `make build`. Help and version work without an engine or database. No private repository access is needed to build, test or install the CLI. [`forkden-private-release.pub`](forkden-private-release.pub) is the public key for verifying Forkden's private worker/MCP releases.

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

Tests cover the Unix transport, incompatible protocol responses, redirect refusal, cancellation, false-check exit codes and preserving local export paths. CI verifies the repository alone on Linux and macOS. [Protocol v1](docs/protocol-v1.md) is public; private engine implementation is outside this repository.

## License

[Apache License 2.0](LICENSE). See [NOTICE](NOTICE). This license applies to the CLI and public protocol/client, not the private engine or backend.
