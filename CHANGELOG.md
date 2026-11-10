# Changelog

The CLI follows semantic versioning; while the major version is 0, a minor release may change commands or output.

## 0.6.0

- New `forkden review` command group: `fork`, `diff`, `check`, `export` and `drop` rehearse a PostgreSQL data change on two local copies (`NAME_base`, read-only, and `NAME_work`) and write `report.md`, `report.json` (schema 1), `changes.sql` and `checks.json` under `./forkden-review/NAME/`. It needs PostgreSQL client tools whose major version equals the target server's; it never writes to the source and never contacts Forkden's cloud.
- `diff NAME --base DSN --work DSN` compares any two databases.
- Exit code 4 means `review diff` wrote a partial report.
- The CLI now links `github.com/jackc/pgx/v5` (MIT) for the `review` commands.
- The shared check failure hint no longer assumes the local engine.

## 0.5.0

- Cloud account login, workspace selection, clone and fork jobs, fork-bound work sessions, readable terminal output.
