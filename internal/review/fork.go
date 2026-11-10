package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	v1 "github.com/forkden/cli/api/v1"
)

// testHookAfterRestore lets tests interrupt fork between restore and freeze.
var testHookAfterRestore func()

// noPassword reports a server that wants a password the DSN, PGPASSWORD and ~/.pgpass did not supply.
func noPassword(cfg *pgx.ConnConfig, err error) bool {
	var pgErr *pgconn.PgError
	if cfg.Password != "" {
		return false
	}
	if errors.As(err, &pgErr) {
		return pgErr.Code == "28P01" || pgErr.Code == "28000"
	}
	return strings.Contains(err.Error(), "password")
}

func missingPassword(cfg *pgx.ConnConfig) error {
	return newError("invalid_input", "no password is available for "+display(cfg),
		"set PGPASSWORD, add the server to ~/.pgpass, or put the password in the DSN (it is never written to disk or printed)",
		"nothing was dumped or created", "review never prompts; pg_dump and pg_restore run with -w")
}

// forkRecord is base.fingerprint: what fork captured, read back by diff and check.
type forkRecord struct {
	Schema          int           `json:"schema"`
	Name            string        `json:"name"`
	CreatedAt       string        `json:"created_at"`
	Source          string        `json:"source"`
	Target          string        `json:"target"`
	Fingerprint     string        `json:"fingerprint"`
	Manifest        string        `json:"manifest_sha256"`
	Tables          []tableDigest `json:"tables"`
	TooLarge        []string      `json:"too_large,omitempty"`
	RestoreWarnings []string      `json:"restore_warnings,omitempty"`
	Timings         forkTimings   `json:"timings"`
}

type tableDigest struct {
	Table  string `json:"table"`
	Rows   int64  `json:"rows"`
	SHA256 string `json:"sha256"`
}

type forkTimings struct {
	DumpMS    int64 `json:"dump_ms"`
	RestoreMS int64 `json:"restore_ms"`
}

type forkPlan struct {
	source, target        string
	sourceCfg, targetCfg  *pgx.ConnConfig
	maintenance           string // database fork connects to on the target server
	dumpTool, restoreTool string
	tooLarge              []*table
	sourceTables          int
}

func (r *runner) fork(ctx context.Context) (any, error) {
	plan, err := r.planFork()
	if err != nil {
		return nil, err
	}
	created := []string{}
	result, err := r.runFork(ctx, plan, &created)
	if err == nil {
		return result, nil
	}
	// Remove only databases this run created; the source is never touched.
	if cleanupErr := r.dropCreated(ctx, plan, created); cleanupErr != nil {
		r.logf("cleanup failed: %s", scrub(cleanupErr.Error()))
		manual := "forkden review drop " + r.cmd.Name + " --to '" + redactDSN(plan.target) + "'"
		var publicErr *v1.Error
		if errors.As(err, &publicErr) {
			publicErr.Message += "\nrecovery: partial databases may remain; run " + manual
		}
	}
	if ctx.Err() != nil {
		return nil, errors.Join(newError("interrupted", "fork was interrupted; partial databases were removed",
			"run the same fork command again", "the source was never written", ""), ctx.Err())
	}
	return nil, err
}

func (r *runner) planFork() (*forkPlan, error) {
	if r.cmd.DSN == "" {
		return nil, newError("invalid_input", "fork needs the source database",
			`pass --dsn "postgres://user@host:5432/db" (the password comes from PGPASSWORD or ~/.pgpass)`, "nothing was created", "")
	}
	plan := &forkPlan{source: r.cmd.DSN, target: r.cmd.To}
	var err error
	if plan.sourceCfg, err = parseDSN(plan.source); err != nil {
		return nil, newError("invalid_input", err.Error(), "check the --dsn value", "nothing was created", "")
	}
	if plan.target == "" {
		// Without --to the copies live on the source server, which then needs CREATEDB.
		plan.target = plan.source
	}
	if plan.targetCfg, err = parseDSN(plan.target); err != nil {
		return nil, newError("invalid_input", err.Error(), "check the --to value", "nothing was created", "")
	}
	plan.maintenance = plan.targetCfg.Database
	if r.cmd.Template && plan.maintenance == plan.sourceCfg.Database {
		plan.maintenance = "postgres"
	}
	overlap := regexp.MustCompile(`^` + regexp.QuoteMeta(r.cmd.Name) + `_(base|work)$`)
	if sameServer(plan.sourceCfg, plan.targetCfg) && overlap.MatchString(plan.sourceCfg.Database) {
		return nil, newError("invalid_input", "the source database "+plan.sourceCfg.Database+" is one of this review's own copies",
			"fork from the real source database, or pick another review name", "nothing was created or dropped", "")
	}
	if r.cmd.Template && !sameServer(plan.sourceCfg, plan.targetCfg) {
		return nil, newError("invalid_input", "--template copies within one server, but --to names another server",
			"drop --to, or drop --template to copy with pg_dump and pg_restore", "nothing was created", "")
	}
	if !r.cmd.Template {
		if plan.dumpTool, err = toolPath("pg_dump"); err != nil {
			return nil, err
		}
		if plan.restoreTool, err = toolPath("pg_restore"); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

func (r *runner) runFork(ctx context.Context, plan *forkPlan, created *[]string) (any, error) {
	targetConn, err := connect(ctx, plan.target, plan.maintenance)
	if err != nil {
		if noPassword(plan.targetCfg, err) {
			return nil, missingPassword(plan.targetCfg)
		}
		return nil, r.traceError("target_unreachable", "cannot connect to the target server "+display(plan.targetCfg),
			"check --to (or --dsn when --to is omitted), PGPASSWORD or ~/.pgpass, and that the server accepts connections", "nothing was created", err)
	}
	defer closeConn(ctx, targetConn)
	if err := r.prepareTarget(ctx, plan, targetConn); err != nil {
		return nil, err
	}
	targetMajor, err := serverMajor(ctx, targetConn)
	if err != nil {
		return nil, r.traceError("target_unreachable", "cannot read the target server version", "", "nothing was created", err)
	}
	sourceConn, err := connect(ctx, plan.source, "")
	if err != nil {
		if noPassword(plan.sourceCfg, err) {
			return nil, missingPassword(plan.sourceCfg)
		}
		return nil, r.traceError("source_unreachable", "cannot connect to the source "+display(plan.sourceCfg),
			"check --dsn, PGPASSWORD or ~/.pgpass; the source role needs only SELECT", "nothing was created", err)
	}
	defer closeConn(ctx, sourceConn)
	sourceMajor, err := serverMajor(ctx, sourceConn)
	if err != nil {
		return nil, r.traceError("source_error", "cannot read the source server version", "", "nothing was created", err)
	}
	if err := r.checkVersions(ctx, plan, sourceMajor, targetMajor); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return nil, r.writeFailed(r.dir, err)
	}

	tx, err := snapshotTx(ctx, sourceConn)
	if err != nil {
		return nil, r.traceError("source_error", "cannot open a read-only snapshot on the source", "", "nothing was created", err)
	}
	defer rollback(ctx, tx)
	if err := r.sourceProfile(ctx, tx); err != nil {
		return nil, err
	}
	sourceTables, err := listTables(ctx, tx)
	if err != nil {
		return nil, r.traceError("source_error", "cannot read the source table inventory", "the source role needs USAGE on every schema and SELECT on every table", "nothing was created", err)
	}
	plan.sourceTables = len(sourceTables)
	sourceInventory, err := inventory(ctx, tx)
	if err != nil {
		return nil, r.traceError("source_error", "cannot read the source object inventory", "", "nothing was created", err)
	}
	var total int64
	for _, t := range sourceTables {
		if t.overCap() {
			t.TooLarge = true
			plan.tooLarge = append(plan.tooLarge, t)
			continue
		}
		total += t.Bytes
	}
	// Two copies plus the archive, roughly the copied data once more.
	expected := total * 3
	r.logf("source %s: %d tables, %s to copy (expected local disk %s)", display(plan.sourceCfg), len(sourceTables), bytesText(total), bytesText(expected))
	for _, t := range plan.tooLarge {
		r.logf("  %s is over the %s / %d-row cap: schema only, listed as unsupported: too_large", t.key(), bytesText(maxTableBytes), int64(maxTableRows))
	}
	if expected > maxTotalBytes {
		return nil, newError("invalid_input", fmt.Sprintf("the copy needs about %s of disk, over the %s limit", bytesText(expected), bytesText(maxTotalBytes)),
			"fork a smaller database or a copy that holds only the tables the task touches", "nothing was created", "")
	}

	timings := forkTimings{}
	var warnings []string
	if r.cmd.Template {
		// TEMPLATE needs the source database free of sessions, including ours.
		rollback(ctx, tx)
		closeConn(ctx, sourceConn)
		if err := r.templateCopy(ctx, plan, targetConn, created); err != nil {
			return nil, err
		}
	} else {
		warnings, err = r.dumpRestore(ctx, plan, tx, targetConn, sourceInventory, created, &timings)
		if err != nil {
			return nil, err
		}
	}
	rollback(ctx, tx)
	closeConn(ctx, sourceConn)

	record, err := r.finishCopies(ctx, plan, targetConn, created)
	if err != nil {
		return nil, err
	}
	record.Source, record.RestoreWarnings, record.Timings = redactDSN(plan.source), warnings, timings
	for _, t := range plan.tooLarge {
		record.TooLarge = append(record.TooLarge, t.key())
	}
	if err := r.writeForkFiles(plan, record); err != nil {
		return nil, err
	}
	target := redactDSN(plan.target)
	r.logf("ready: %s (writable) and %s (read-only)\nreuse with: --to '%s'\nagent connection: %s",
		r.work(), r.base(), target, withDatabase(target, r.work()))
	if plan.targetCfg.Password != "" && os.Getenv("PGPASSWORD") == "" && target != plan.target {
		r.logf("note: the password in the DSN is not stored; diff, check and drop read it from PGPASSWORD or ~/.pgpass")
	}
	result := map[string]any{
		"name": r.cmd.Name, "status": "forked", "target": target,
		"base_database": r.base(), "work_database": r.work(),
		"work_dsn": withDatabase(target, r.work()), "fingerprint": record.Fingerprint,
		"tables": plan.sourceTables, "timings": timings,
		"next": "forkden review diff " + r.cmd.Name,
	}
	if len(record.TooLarge) > 0 {
		result["too_large"] = record.TooLarge
	}
	if len(warnings) > 0 {
		result["restore_warnings"] = warnings
	}
	return result, nil
}

// prepareTarget refuses or, with --force, removes an earlier review of the same name.
func (r *runner) prepareTarget(ctx context.Context, plan *forkPlan, conn *pgx.Conn) error {
	existing, err := existingDatabases(ctx, conn, r.base(), r.work())
	if err != nil {
		return r.traceError("target_unreachable", "cannot list databases on the target server", "", "nothing was created", err)
	}
	if len(existing) > 0 && !r.cmd.Force {
		return newError("invalid_input", strings.Join(existing, " and ")+" already exist on "+display(plan.targetCfg),
			"run forkden review diff "+r.cmd.Name+" to review it, or fork again with --force (drops both copies and the local report directory)",
			"nothing was changed", "")
	}
	if r.cmd.Force {
		if err := r.dropMarked(ctx, conn, existing); err != nil {
			return err
		}
		if err := os.RemoveAll(r.dir); err != nil {
			return r.writeFailed(r.dir, err)
		}
		return nil
	}
	// A dropped review can leave reports of databases that no longer exist; never mix generations.
	for _, name := range append(slices.Clone(artifactFiles), "target", "base.fingerprint") {
		if err := os.Remove(filepath.Join(r.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return r.writeFailed(r.dir, err)
		}
	}
	return nil
}

func (r *runner) checkVersions(ctx context.Context, plan *forkPlan, sourceMajor, targetMajor int) error {
	ctxLine := fmt.Sprintf("source PostgreSQL %d, target PostgreSQL %d", sourceMajor, targetMajor)
	if sourceMajor < 12 {
		return newError("invalid_input", "the source runs PostgreSQL "+strconv.Itoa(sourceMajor)+"; review supports 12 and later", "", "nothing was created", ctxLine)
	}
	if targetMajor < sourceMajor {
		return newError("invalid_input", "the target server is older than the source",
			"use a target server whose major version is at least the source's (for example Docker postgres:"+strconv.Itoa(sourceMajor)+")", "nothing was created", ctxLine)
	}
	if r.cmd.Template {
		return nil
	}
	clientMajor, err := toolMajor(ctx, plan.dumpTool)
	if err != nil {
		return r.traceError("tool_missing", "cannot run pg_dump --version", "reinstall the PostgreSQL client tools", "nothing was created", err)
	}
	if restoreMajor, err := toolMajor(ctx, plan.restoreTool); err != nil || restoreMajor != clientMajor {
		return newError("tool_missing", "pg_dump and pg_restore on PATH come from different PostgreSQL versions", "put one client-tools directory first on PATH", "nothing was created", "")
	}
	ctxLine += fmt.Sprintf(", client tools %d (%s)", clientMajor, plan.dumpTool)
	if clientMajor != targetMajor {
		return newError("tool_missing", "the client tools' major version must equal the target server's",
			fmt.Sprintf("install postgresql-client-%d (or point --to at a PostgreSQL %d server)", targetMajor, clientMajor), "nothing was created", ctxLine)
	}
	return nil
}

var versionPattern = regexp.MustCompile(`\(PostgreSQL\) (\d+)`)

func toolMajor(ctx context.Context, path string) (int, error) {
	out, err := toolOutput(ctx, path, "--version")
	if err != nil {
		return 0, err
	}
	match := versionPattern.FindStringSubmatch(out)
	if match == nil {
		return 0, errors.New("unrecognized version output: " + strings.TrimSpace(out))
	}
	return strconv.Atoi(match[1])
}

func serverMajor(ctx context.Context, conn *pgx.Conn) (int, error) {
	var num int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&num); err != nil {
		return 0, err
	}
	return num / 10000, nil
}

// sourceProfile refuses sources whose restored copy could reach other systems.
func (r *runner) sourceProfile(ctx context.Context, tx pgx.Tx) error {
	var reasons []string
	err := tx.QueryRow(ctx, `SELECT array_remove(ARRAY[
		CASE WHEN EXISTS (SELECT FROM pg_foreign_table) THEN 'foreign tables' END,
		CASE WHEN EXISTS (SELECT FROM pg_foreign_server) THEN 'foreign servers' END,
		CASE WHEN EXISTS (SELECT FROM pg_event_trigger) THEN 'event triggers' END,
		(SELECT 'extensions ' || string_agg(extname, ', ') FROM pg_extension WHERE extname IN ('postgres_fdw', 'dblink', 'file_fdw', 'pg_net', 'http')),
		(SELECT 'untrusted languages ' || string_agg(lanname, ', ') FROM pg_language WHERE lanispl AND NOT lanpltrusted)
	], NULL)`).Scan(&reasons)
	if err != nil {
		return r.traceError("source_error", "cannot inspect the source's extensions and foreign objects", "", "nothing was created", err)
	}
	if len(reasons) > 0 {
		return newError("unsupported_source", "unsupported: external_reach — the source has "+strings.Join(reasons, "; "),
			"fork a copy without objects that reach other systems", "nothing was dumped or created",
			"a restored copy of these objects could read from or write to systems outside the copy")
	}
	return nil
}

// inventory lists the objects a restore must reproduce; pg_restore warnings are
// harmless only when this set is complete afterwards.
func inventory(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT c.relkind::text || ' ' || n.nspname || '.' || c.relname FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind IN ('r', 'p', 'v', 'm', 'S') AND `+userSchemas+`
		UNION ALL SELECT 'f ' || n.nspname || '.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')'
			FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE `+userSchemas+`
		UNION ALL SELECT 'x ' || extname FROM pg_extension`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []string
	for rows.Next() {
		var item string
		if err := rows.Scan(&item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	slices.Sort(items)
	return items, rows.Err()
}

func (r *runner) dumpRestore(ctx context.Context, plan *forkPlan, tx pgx.Tx, targetConn *pgx.Conn, sourceInventory []string, created *[]string, timings *forkTimings) ([]string, error) {
	// The exporting transaction stays open until pg_dump exits (M1).
	_, _ = tx.Exec(ctx, "SET LOCAL idle_in_transaction_session_timeout = 0")
	var snapshot string
	if err := tx.QueryRow(ctx, "SELECT pg_export_snapshot()").Scan(&snapshot); err != nil {
		return nil, r.traceError("source_error", "the source refused to export a snapshot", "the source must not be a hot standby without hot_standby_feedback", "nothing was created", err)
	}
	work := filepath.Join(r.dir, ".tmp-fork")
	if err := os.RemoveAll(work); err != nil {
		return nil, r.writeFailed(work, err)
	}
	if err := os.Mkdir(work, 0o700); err != nil {
		return nil, r.writeFailed(work, err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	archive := filepath.Join(work, "source.dump")
	args := []string{"--format=custom", "--no-owner", "--no-privileges", "--snapshot=" + snapshot, "--file=" + archive}
	for _, t := range plan.tooLarge {
		args = append(args, "--exclude-table-data="+dumpPattern(t.Schema, t.Name))
	}
	args = append(args, "--dbname="+redactDSN(plan.source))
	r.logf("dump: pg_dump %s", strings.Join(args, " "))
	start := time.Now()
	stderr, err := runTool(ctx, plan.dumpTool, args, plan.sourceCfg.Password)
	timings.DumpMS = time.Since(start).Milliseconds()
	_ = appendFile(filepath.Join(r.dir, "fork.log"), "pg_dump stderr:\n"+stderr+"\n")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, r.traceError("dump_failed", "pg_dump failed\n"+tail(stderr, 20), "fix the cause above and run fork again", "nothing was created on the target", err)
	}
	r.logf("dump: done in %s", time.Duration(timings.DumpMS)*time.Millisecond)

	if err := createDatabase(ctx, targetConn, r.base(), ""); err != nil {
		return nil, r.traceError("restore_failed", "cannot create "+r.base()+" on the target", "the target role needs CREATEDB", "nothing was created", err)
	}
	*created = append(*created, r.base())
	jobs := strconv.Itoa(min(4, runtime.NumCPU()))
	restoreArgs := []string{"--no-owner", "--no-privileges", "--no-tablespaces", "--no-publications", "--no-subscriptions", "-j", jobs,
		"--dbname=" + withDatabase(redactDSN(plan.target), r.base()), archive}
	r.logf("restore: pg_restore into %s", r.base())
	start = time.Now()
	stderr, restoreErr := runTool(ctx, plan.restoreTool, restoreArgs, plan.targetCfg.Password)
	timings.RestoreMS = time.Since(start).Milliseconds()
	_ = appendFile(filepath.Join(r.dir, "fork.log"), "pg_restore stderr:\n"+stderr+"\n")
	if testHookAfterRestore != nil {
		testHookAfterRestore()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var warnings []string
	if restoreErr != nil {
		baseConn, err := connect(ctx, plan.target, r.base())
		if err != nil {
			return nil, r.traceError("restore_failed", "pg_restore failed and the copy cannot be inspected\n"+tail(stderr, 20), "", "the partial copy is removed", err)
		}
		restored, err := inventory(ctx, baseConn)
		closeConn(ctx, baseConn)
		if err != nil {
			return nil, r.traceError("restore_failed", "pg_restore failed and the copy cannot be inspected", "", "the partial copy is removed", err)
		}
		if missing := difference(sourceInventory, restored); len(missing) > 0 {
			return nil, r.traceError("restore_failed", "missing_capture: the copy lacks "+strings.Join(missing, ", ")+"\n"+tail(stderr, 20),
				"install the missing extensions on the target server, or fork a copy without them", "the partial copy is removed", restoreErr)
		}
		for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				warnings = append(warnings, line)
			}
		}
		r.logf("restore: finished with %d warnings; every object was restored", len(warnings))
	}
	return warnings, nil
}

func (r *runner) templateCopy(ctx context.Context, plan *forkPlan, targetConn *pgx.Conn, created *[]string) error {
	r.logf("copy: CREATE DATABASE %s TEMPLATE %s", r.base(), plan.sourceCfg.Database)
	if err := createDatabase(ctx, targetConn, r.base(), plan.sourceCfg.Database); err != nil {
		return r.traceError("restore_failed", "CREATE DATABASE ... TEMPLATE failed", "--template needs CREATEDB and no other sessions on the source database; drop --template to copy with pg_dump instead", "nothing was created", err)
	}
	*created = append(*created, r.base())
	return nil
}

// finishCopies runs the fixed order: ANALYZE, marker, fingerprint, TEMPLATE copy, marker, freeze last.
func (r *runner) finishCopies(ctx context.Context, plan *forkPlan, targetConn *pgx.Conn, created *[]string) (*forkRecord, error) {
	baseConn, err := connect(ctx, plan.target, r.base())
	if err != nil {
		return nil, r.traceError("target_unreachable", "cannot connect to "+r.base(), "", "the partial copy is removed", err)
	}
	if _, err := baseConn.Exec(ctx, "ANALYZE"); err != nil {
		r.logf("warning: ANALYZE on %s failed: %s", r.base(), scrub(publicCause(err)))
	}
	if _, err := baseConn.Exec(ctx, "COMMENT ON DATABASE "+quoteIdent(r.base())+" IS "+quoteLiteral(r.marker())); err != nil {
		closeConn(ctx, baseConn)
		return nil, r.traceError("freeze_failed", "cannot mark "+r.base(), "", "the partial copy is removed", err)
	}
	record, err := r.fingerprintDatabase(ctx, baseConn, plan.tooLarge)
	closeConn(ctx, baseConn)
	if err != nil {
		return nil, r.traceError("freeze_failed", "cannot fingerprint "+r.base(), "", "the partial copy is removed", err)
	}
	r.logf("copy: %s TEMPLATE %s", r.work(), r.base())
	if err := createDatabase(ctx, targetConn, r.work(), r.base()); err != nil {
		return nil, r.traceError("restore_failed", "cannot create "+r.work(), "", "the partial copies are removed", err)
	}
	*created = append(*created, r.work())
	if _, err := targetConn.Exec(ctx, "COMMENT ON DATABASE "+quoteIdent(r.work())+" IS "+quoteLiteral(r.marker())); err != nil {
		return nil, r.traceError("freeze_failed", "cannot mark "+r.work(), "", "the partial copies are removed", err)
	}
	if _, err := targetConn.Exec(ctx, "ALTER DATABASE "+quoteIdent(r.base())+" SET default_transaction_read_only = on"); err != nil {
		return nil, r.traceError("freeze_failed", "cannot make "+r.base()+" read-only", "", "the partial copies are removed", err)
	}
	record.Name, record.Target = r.cmd.Name, redactDSN(plan.target)
	return record, nil
}

func (r *runner) fingerprintDatabase(ctx context.Context, conn *pgx.Conn, tooLarge []*table) (*forkRecord, error) {
	tx, err := snapshotTx(ctx, conn)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	manifest, err := readManifest(ctx, tx)
	if err != nil {
		return nil, err
	}
	tables, err := listTables(ctx, tx)
	if err != nil {
		return nil, err
	}
	excluded := map[string]bool{}
	for _, t := range tooLarge {
		excluded[t.key()] = true
	}
	record := &forkRecord{Schema: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Manifest: manifestDigest(manifest)}
	for _, t := range tables {
		if excluded[t.key()] {
			t.TooLarge = true
			continue
		}
		if err := digestTable(ctx, tx, t); err != nil {
			return nil, err
		}
		record.Tables = append(record.Tables, tableDigest{Table: t.key(), Rows: t.Rows, SHA256: t.SHA256})
	}
	record.Fingerprint = fingerprint(record.Manifest, tables)
	return record, nil
}

func (r *runner) writeForkFiles(plan *forkPlan, record *forkRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(r.dir, "base.fingerprint", append(data, '\n')); err != nil {
		return r.writeFailed(r.dir, err)
	}
	if err := writeAtomic(r.dir, "target", []byte(redactDSN(plan.target)+"\n")); err != nil {
		return r.writeFailed(r.dir, err)
	}
	return nil
}

func (r *runner) dropCreated(ctx context.Context, plan *forkPlan, created []string) error {
	if len(created) == 0 {
		return nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	conn, err := connect(cleanup, plan.target, plan.maintenance)
	if err != nil {
		return err
	}
	defer closeConn(cleanup, conn)
	var errs []error
	for _, name := range slices.Backward(created) {
		errs = append(errs, dropDatabase(cleanup, conn, name))
	}
	return errors.Join(errs...)
}

func existingDatabases(ctx context.Context, conn *pgx.Conn, names ...string) ([]string, error) {
	rows, err := conn.Query(ctx, "SELECT datname FROM pg_database WHERE datname = ANY($1) ORDER BY datname", names)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func createDatabase(ctx context.Context, conn *pgx.Conn, name, template string) error {
	statement := "CREATE DATABASE " + quoteIdent(name)
	if template != "" {
		statement += " TEMPLATE " + quoteIdent(template)
	}
	_, err := conn.Exec(ctx, statement)
	return err
}

func dropDatabase(ctx context.Context, conn *pgx.Conn, name string) error {
	if _, err := conn.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", name); err != nil {
		return err
	}
	_, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(name))
	return err
}

// dumpPattern quotes both parts so pg_dump matches the exact names, wildcards included.
func dumpPattern(schema, name string) string {
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	return quote(schema) + "." + quote(name)
}

func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func difference(want, have []string) []string {
	var missing []string
	for _, item := range want {
		if _, found := slices.BinarySearch(have, item); !found {
			missing = append(missing, item)
		}
	}
	return missing
}

func bytesText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
