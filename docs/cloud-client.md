# Account and copy-job client

This working-tree slice builds independently as `0.3.0-dev`. It is not a published release or a deployed hosted service. The published `v0.2.0` retains the local Unix-socket protocol.

## Workflow

Build the CLI with `make build`. Run the configured account backend with the built portal on its canonical origin. Create a workspace/project, pair a self-hosted private worker and link a display profile to it in the portal. Configure DB references only in that worker's private bindings. Then:

```sh
./bin/forkden --api_url http://127.0.0.1:8080 login
./bin/forkden whoami
./bin/forkden cloud org list
./bin/forkden cloud project list --org ORG_ID
./bin/forkden cloud project use PROJECT_ID --org ORG_ID
./bin/forkden cloud project current
./bin/forkden cloud db list
./bin/forkden cloud clone create baseline --profile PROFILE_ID --wait
./bin/forkden cloud fork create experiment --clone RESOURCE_ID --version 1 --ttl 1h --wait
./bin/forkden cloud job list
./bin/forkden cloud job status JOB_ID
./bin/forkden logout
```

Replace IDs with API output. A fork uses the succeeded clone job's **resource ID**, not its job ID. Snapshot version 1 is immutable. Fork TTL is 60–86400 whole seconds. `cloud clone list/show` and `cloud fork list/show` return creation-job metadata. An expiry timestamp does not prove physical cleanup. Local commands without `cloud` remain on the Unix transport; `--socket` is refused with cloud commands. Cloud query execution, refresh/delete, running cancellation and managed provisioning are pending.

Global flags precede commands. `--json` writes one compact `{ok,data,error?}` envelope to stdout; progress and the human-readable login code go to stderr. No account/device bearer is printed. `job status` reports valid failed/uncertain states with success; `job wait` returns nonzero when a job ends without success.

## Login and credentials

Login is a first-party JSON device-consent flow inspired by [RFC 8628](https://www.rfc-editor.org/rfc/rfc8628), rather than a general OAuth authorization server. The CLI starts it only when the user runs login. The device credential is high entropy and never displayed. The separate terminal code expires after ten minutes. The browser requires GitHub login, same-origin cookie/CSRF and an explicit approve/decline action; the confirmation screen identifies the account and requires checking that the code matches a login the user started.

The CLI polls at the server interval (initially five seconds), increases by five seconds on `slow_down`, and backs off on transport failure. Denial, expiry, replay or another error ends polling. Approval is redeemed once into a seven-day `fda_` account credential with fixed `metadata:read jobs:write` capability. No refresh token or provider token is stored. Losing a successful token response ends the flow; an inaccessible unclaimed credential expires normally rather than being reissued by replay.

macOS Keychain, Linux/BSD Secret Service and Windows Credential Manager are supported by the public [go-keyring adapter](https://github.com/zalando/go-keyring). An unlocked OS keyring is required, including with `login --no-browser`. There is no plaintext fallback. A failure to persist a received grant attempts bounded server revocation. If that cleanup cannot reach the server, the credential expires in seven days. Logout first revokes the current account token, then clears local login settings and the keyring entry. It leaves browser sessions and worker credentials intact. If server revocation cannot be confirmed, local settings remain so logout can be retried.

`--api_url` / `FORKDEN_API_URL` specify an HTTPS origin, or loopback HTTP for development. The initial login requires an explicit configured API origin; no production address is assumed. Login pins it. An override cannot send the saved credential to a different server: log out first or use a separate `--config_dir`. Redirects are refused; response bodies are capped at 1 MiB, requests at 16 KiB and HTTP I/O at ten seconds. The account transport uses normal OS certificate validation and standard proxy environment settings.

Settings default to the OS config directory plus `forkden/cli`. `--config_dir` or `FORKDEN_CLI_CONFIG_DIR` overrides it. `cloud.json` contains only `{api_url,credential_id,user_id,org_id,project_id}`. It never stores tokens, DB connection information or secret-manager reference names. The OS keyring record binds the token to its canonical API origin; changing the origin in settings is refused before any authenticated request. Unix directory/file permissions are `0700`/`0600`; symlink config files are refused. Login/logout/project-selection writers serialize with `.write-lock`. After a writer is killed, remove that lock and any `.cloud.json.tmp` only once no writer remains. Read commands and polling can run concurrently.

## Jobs and capabilities

The account bearer is accepted only for CLI session/logout, organization/project/profile/worker metadata reads, job reads/create and queued cancellation. It cannot administer orgs/projects/profiles/members, issue worker pairings, approve another CLI grant, substitute for a cookie or claim worker jobs. Mixed cookie/bearer requests are rejected. Server membership and role checks run again on every operation; local selection is navigation metadata, not authorization.

Create prints a generated safe request ID **before** the mutation. Reuse it with `--request_id` to repeat the identical intent after a lost response; the server returns the same job. Changed meaning conflicts. Mutations never retry automatically. `--wait` and `cloud job wait JOB_ID --timeout 20m` poll every five seconds, with a bounded timeout. Interrupting/timing out polling leaves work active. `cloud job cancel JOB_ID` affects queued work only.

The boundary consists of names, opaque org/project/profile/worker/resource IDs, clone version, TTL and authoritative lifecycle metadata. SQL, input values, rows, source/admin URLs, env names, private PostgreSQL identities and raw errors have no fields. Fixed prototype limits are server-enforced and are not paid entitlements. Managed provisioning, cloud SQL/history, self-hosted packaging and production edge/distributed rate limits remain separate work.
