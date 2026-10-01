# Engine protocol v1

The public Go contract and client are in [`api/v1`](../api/v1). The private engine implements this contract without publishing its workflow code.

## Transport and versioning

HTTP/1.1 over a Unix socket only. Host is `forkden`; command requests use `Content-Type: application/json`. There is no TCP endpoint, CORS support, account login or token-bearing remote API in this prototype. The engine holds its state lock and executes at most one command at a time. Concurrent commands receive `409 engine_busy` before execution.

`GET /v1/status` returns engine/protocol version and transport. `POST /v1/commands` accepts one request:

```json
{"api_version":"1","action":"profile.add","params":{"name":"sample","url_env":"FORKDEN_SOURCE_URL"}}
```

`api_version` identifies the wire protocol, independently of binary versions. Unsupported versions/actions have stable errors. Unknown request/parameter fields, concatenated JSON and oversized requests are rejected. Breaking wire changes require a new protocol version. New response fields and additional actions can be additive; clients must tolerate additional response fields and must not assume an older engine supports new actions.

## Actions

| Action | Params |
|---|---|
| `profile.add`, `profile.update` | `name`, `url_env` |
| `profile.list` | `{}` |
| `profile.show`, `profile.check`, `profile.remove` | `name` |
| `fork.create` | `source`, `target_env`, `ttl_seconds` (integer 1–86400) |
| `fork.list`, `fork.prune` | `{}` |
| `fork.close`, `history`, `review`, `export` | `id` |
| `query`, `check` | `fork_id`, `sql` |

No action accepts a database URL or client-side file path. Environment references are resolved on the engine. SQL may contain user data and is part of the engine audit.

## Response

```json
{"api_version":"1","ok":true,"data":{"name":"sample","url_env":"FORKDEN_SOURCE_URL","database":"sample_db","created_at":"2026-10-02T00:00:00Z"}}
```

Error responses preserve public operation data when available, including evaluated false checks:

```json
{"api_version":"1","ok":false,"data":null,"error":{"code":"invalid_input","message":"forkden: invalid input"}}
```

Profile results contain name, reference, database and creation time. Fork results contain identity, source, lifecycle, target reference, expiry and copy scope. Operation results contain SQL, transaction outcome, bounded result data, truncation and check state. Review contains per-table counts/samples, explicit coverage and operation history. Private cluster fingerprints, connection URLs and generated passwords are excluded.

`export` returns `{"sql":"...","report":{...}}`. The CLI writes `changes.sql` and `report.json` in a new local directory; the engine receives no filesystem destination. Export does not apply changes to the source.

| Code | HTTP status | Meaning |
|---|---|---|
| `invalid_input`, `unsupported_action`, `unsupported_version` | 400 | Rejected input/contract |
| `not_found` | 404 | Unknown profile/fork/endpoint |
| `inactive`, `engine_busy` | 409 | Inactive fork or command not started due to another operation |
| `check_failed` | 422 | Check evaluated false; operation data retained |
| `cancelled` | 408 | Cancellation/deadline; inspect audit before retrying |
| `operation_failed`, `internal_error`, `response_too_large` | 500 | Operation/encoding/response-limit failure; inspect state before retrying |

Limits: 256 KiB request, 16 MiB response, 90-second command deadline, two-minute client timeout. SQL has a 64 KiB bound and the engine applies narrower query/expiry deadlines. JSON errors do not expose raw database diagnostics. The client refuses incompatible/malformed envelopes and redirects and performs no application retries.

## Account and agent scope

Socket access grants the local OS user access to this engine's registry; there is no per-account tenant authorization. A future backend and fork-bound MCP session need separate identity, ownership, revocation and entitlement checks. Client-side flags or an OSS license cannot enforce paid access. Do not expose this local handler as a hosted API.
