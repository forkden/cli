package review

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
)

// These tests need a PostgreSQL server whose role may create databases, plus pg_dump and
// pg_restore of the same major version on PATH:
//
//	FORKDEN_REVIEW_TEST_DSN=postgres://postgres:postgres@localhost:5433/postgres go test ./internal/review/
//
// They are not parallel: the size-cap test lowers package-level caps.

var testSeq atomic.Int64

type fixture struct {
	t      *testing.T
	admin  string // maintenance DSN
	source string // source database DSN
	name   string // review name
	root   string
}

func newFixture(t *testing.T, extraSQL ...string) *fixture {
	t.Helper()
	admin := os.Getenv("FORKDEN_REVIEW_TEST_DSN")
	if admin == "" {
		t.Skip("set FORKDEN_REVIEW_TEST_DSN to a PostgreSQL server (CREATEDB) to run review database tests")
	}
	f := &fixture{t: t, admin: admin, root: t.TempDir()}
	// The target file never stores a password; later commands read it like a developer's shell would.
	if cfg, err := parseDSN(admin); err == nil && cfg.Password != "" {
		t.Setenv("PGPASSWORD", cfg.Password)
	}
	n := testSeq.Add(1)
	f.name = fmt.Sprintf("rv%d_%d", os.Getpid()%10000, n)
	sourceDB := f.name + "_src"
	f.exec(admin, "DROP DATABASE IF EXISTS "+quoteIdent(sourceDB))
	f.exec(admin, "CREATE DATABASE "+quoteIdent(sourceDB))
	f.source = withDatabase(admin, sourceDB)
	fixtureSQL, err := os.ReadFile(filepath.Join("testdata", "fixture.sql"))
	if err != nil {
		t.Fatal(err)
	}
	f.exec(f.source, string(fixtureSQL))
	for _, sql := range extraSQL {
		f.exec(f.source, sql)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		conn, err := connect(ctx, admin, "")
		if err != nil {
			return
		}
		defer closeConn(ctx, conn)
		for _, db := range []string{f.name + "_base", f.name + "_work", sourceDB, f.name + "_rt"} {
			_ = dropDatabase(ctx, conn, db)
		}
	})
	return f
}

func (f *fixture) exec(dsn, sql string) {
	f.t.Helper()
	ctx := context.Background()
	conn, err := connect(ctx, dsn, "")
	if err != nil {
		f.t.Fatalf("connect: %v", err)
	}
	defer closeConn(ctx, conn)
	// Simple protocol runs a multi-statement script in one call.
	if _, err := conn.Exec(ctx, sql, pgx.QueryExecModeSimpleProtocol); err != nil {
		f.t.Fatalf("exec %q: %v", firstLine(sql), err)
	}
}

func (f *fixture) query(dsn, sql string) string {
	f.t.Helper()
	ctx := context.Background()
	conn, err := connect(ctx, dsn, "")
	if err != nil {
		f.t.Fatalf("connect: %v", err)
	}
	defer closeConn(ctx, conn)
	var value *string
	if err := conn.QueryRow(ctx, sql).Scan(&value); err != nil {
		f.t.Fatalf("query %q: %v", sql, err)
	}
	if value == nil {
		return ""
	}
	return *value
}

func (f *fixture) cmd(action string) Command {
	return Command{Action: action, Name: f.name, Root: f.root}
}

func (f *fixture) fork() map[string]any {
	f.t.Helper()
	cmd := f.cmd("fork")
	cmd.DSN, cmd.To = f.source, f.admin
	result, err := Run(context.Background(), cmd, nil)
	if err != nil {
		f.t.Fatalf("fork: %v", err)
	}
	return result.(map[string]any)
}

func (f *fixture) work() string { return withDatabase(f.admin, f.name+"_work") }
func (f *fixture) base() string { return withDatabase(f.admin, f.name+"_base") }
func (f *fixture) dir() string  { return filepath.Join(f.root, f.name) }

func (f *fixture) diff() (*Report, error) {
	f.t.Helper()
	_, err := Run(context.Background(), f.cmd("diff"), nil)
	var report Report
	if readErr := readJSON(filepath.Join(f.dir(), "report.json"), &report); readErr != nil {
		f.t.Fatalf("diff wrote no report.json: %v (diff error %v)", readErr, err)
	}
	return &report, err
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func tableReport(t *testing.T, r *Report, name string) TableReport {
	t.Helper()
	i := slices.IndexFunc(r.Tables, func(tr TableReport) bool { return tr.Name == name })
	if i < 0 {
		t.Fatalf("report has no table %s: %+v", name, r.Tables)
	}
	return r.Tables[i]
}

func TestGoldenDiff(t *testing.T) {
	f := newFixture(t)
	result := f.fork()
	if result["work_dsn"] != f.workRedacted() {
		t.Errorf("fork work_dsn = %v", result["work_dsn"])
	}
	f.exec(f.work(), `UPDATE customers SET country = 'TR' WHERE country IS NULL;
		INSERT INTO orders VALUES (4, 1, 'pending', 3.00);
		DELETE FROM order_items WHERE order_id = 3;
		UPDATE orders SET status = 'shipped' WHERE id = 2`)
	report, err := f.diff()
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if report.Status != "ok" || report.Totals != (Totals{TablesChanged: 3, Inserted: 1, Updated: 3, Deleted: 1}) {
		t.Fatalf("report = %s %+v", report.Status, report.Totals)
	}
	customers := tableReport(t, report, "customers")
	if customers.Updated != 2 || len(customers.Samples) != 2 || compactJSON(customers.Samples[0].PK) != `{"id":2}` {
		t.Errorf("customers = %+v", customers)
	}
	items := tableReport(t, report, "order_items")
	if items.Deleted != 1 || compactJSON(items.Samples[0].PK) != `{"order_id":3,"line":1}` || len(items.Samples[0].New) != 0 {
		t.Errorf("order_items = %+v", items)
	}
	md, _ := os.ReadFile(filepath.Join(f.dir(), "report.md"))
	for _, want := range []string{"Status: **ok**", "| 3 | 1 | 3 | 1 |", "country: `null` → `\"TR\"`", "status: `\"pending\"` → `\"shipped\"`"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("report.md lacks %q", want)
		}
	}
	sql, _ := os.ReadFile(filepath.Join(f.dir(), "changes.sql"))
	for _, want := range []string{
		`UPDATE "public"."customers" SET "country" = 'TR'::text WHERE "id" = '2'::integer;`,
		`DELETE FROM "public"."order_items" WHERE "order_id" = '3'::bigint AND "line" = '1'::integer;`,
		`INSERT INTO "public"."orders" ("id", "customer_id", "status", "total") VALUES ('4'::bigint, '1'::integer, 'pending'::text, '3.00'::numeric(10,2));`,
	} {
		if !strings.Contains(string(sql), want) {
			t.Errorf("changes.sql lacks %s\n%s", want, sql)
		}
	}
	for _, name := range []string{"report.json", "report.md", "changes.sql", "manifest.json", "target", "base.fingerprint"} {
		info, err := os.Stat(filepath.Join(f.dir(), name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, %v; want 0600", name, info, err)
		}
	}
	if info, err := os.Stat(f.dir()); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("review directory mode = %v, %v; want 0700", info, err)
	}
	if _, err := readGeneration(f.dir()); err != nil {
		t.Errorf("generation incomplete: %v", err)
	}
}

func (f *fixture) workRedacted() string { return withDatabase(redactDSN(f.admin), f.name+"_work") }

func TestForkFreezesBaseLastAndMarksBothCopies(t *testing.T) {
	f := newFixture(t)
	f.fork()
	for _, db := range []string{f.name + "_base", f.name + "_work"} {
		if got := f.query(f.admin, "SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = '"+db+"'"); got != "forkden-review:"+f.name {
			t.Errorf("%s marker = %q", db, got)
		}
	}
	if got := f.query(f.base(), "SHOW default_transaction_read_only"); got != "on" {
		t.Errorf("base read-only = %q", got)
	}
	if got := f.query(f.work(), "SHOW default_transaction_read_only"); got != "off" {
		t.Errorf("work read-only = %q; the freeze must come after the TEMPLATE copy", got)
	}
	target, _ := os.ReadFile(filepath.Join(f.dir(), "target"))
	if strings.TrimSpace(string(target)) != redactDSN(f.admin) || strings.Contains(string(target), "password") {
		t.Errorf("target file = %q", target)
	}
	// Right after fork the two copies are identical, so diff is clean and the fingerprints match.
	report, err := f.diff()
	if err != nil || report.Totals.TablesChanged != 0 || report.Base.Fingerprint != report.Work.Fingerprint || report.BaseChanged {
		t.Fatalf("fresh fork diff = %v %+v", err, report)
	}
}

func TestForkRefusesExistingNameUnlessForced(t *testing.T) {
	f := newFixture(t)
	f.fork()
	cmd := f.cmd("fork")
	cmd.DSN, cmd.To = f.source, f.admin
	if _, err := Run(context.Background(), cmd, nil); code(err) != "invalid_input" {
		t.Fatalf("second fork = %v; want invalid_input", err)
	}
	f.exec(f.work(), "UPDATE customers SET email = 'changed@example.com'")
	cmd.Force = true
	if _, err := Run(context.Background(), cmd, nil); err != nil {
		t.Fatalf("fork --force: %v", err)
	}
	if got := f.query(f.work(), "SELECT email FROM customers WHERE id = 1"); got != "ada@example.com" {
		t.Errorf("--force must recreate the copies; email = %q", got)
	}
}

func TestForkRefusesItsOwnCopyAsSource(t *testing.T) {
	f := newFixture(t)
	f.fork()
	cmd := f.cmd("fork")
	cmd.DSN, cmd.To, cmd.Force = f.work(), f.admin, true
	if _, err := Run(context.Background(), cmd, nil); code(err) != "invalid_input" {
		t.Fatalf("fork from own _work = %v; want invalid_input", err)
	}
	if got := f.query(f.admin, "SELECT count(*)::text FROM pg_database WHERE datname LIKE '"+f.name+"\\_%'"); got != "3" {
		t.Errorf("nothing may be dropped; databases = %s", got)
	}
}

func TestForkWithoutPasswordFailsBeforeDump(t *testing.T) {
	f := newFixture(t)
	cfg, err := parseDSN(f.admin)
	if err != nil || cfg.Password == "" {
		t.Skip("needs a FORKDEN_REVIEW_TEST_DSN with a password")
	}
	t.Setenv("PGPASSWORD", "")
	t.Setenv("PGPASSFILE", filepath.Join(t.TempDir(), "none"))
	cmd := f.cmd("fork")
	cmd.DSN, cmd.To = redactDSN(f.source), redactDSN(f.admin)
	if _, err := Run(context.Background(), cmd, nil); code(err) != "invalid_input" || !strings.Contains(err.Error(), "no password") {
		t.Fatalf("fork without password = %v; want invalid_input", err)
	}
	if got := f.query(f.admin, "SELECT count(*)::text FROM pg_database WHERE datname = '"+f.name+"_base'"); got != "0" {
		t.Error("nothing may be created without a password")
	}
}

func TestInterruptedForkRemovesPartialCopies(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	testHookAfterRestore = cancel
	t.Cleanup(func() { testHookAfterRestore = nil })
	cmd := f.cmd("fork")
	cmd.DSN, cmd.To = f.source, f.admin
	_, err := Run(ctx, cmd, nil)
	if code(err) != "interrupted" || !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted fork = %v", err)
	}
	if got := f.query(f.admin, "SELECT count(*)::text FROM pg_database WHERE datname IN ('"+f.name+"_base', '"+f.name+"_work')"); got != "0" {
		t.Errorf("partial copies left behind: %s", got)
	}
}

func TestDiffReadsOneSnapshot(t *testing.T) {
	f := newFixture(t)
	f.fork()
	f.exec(f.work(), "UPDATE orders SET status = 'cancelled' WHERE id = 1")
	ctx := context.Background()
	r := &runner{cmd: f.cmd("diff"), progress: nopWriter{}, dir: f.dir()}
	base, err := r.openSide(ctx, f.base(), "base")
	if err != nil {
		t.Fatal(err)
	}
	defer base.close(ctx)
	work, err := r.openSide(ctx, f.work(), "work")
	if err != nil {
		t.Fatal(err)
	}
	defer work.close(ctx)
	// Committed after the snapshot opened: must not appear in this report.
	f.exec(f.work(), "UPDATE orders SET status = 'refunded' WHERE id = 3")
	if err := os.MkdirAll(f.dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	changes, err := newChangeWriter(filepath.Join(t.TempDir(), "changes.sql"), f.name, "now", "b", "w")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = changes.close() }()
	report := &Report{}
	if err := r.compare(ctx, base, work, nil, report, changes); err != nil {
		t.Fatal(err)
	}
	if orders := tableReport(t, report, "orders"); orders.Updated != 1 || !strings.Contains(string(orders.Samples[0].New), "cancelled") {
		t.Fatalf("orders = %+v; want only the write committed before the snapshot", orders)
	}
}

func TestBaseChangedMakesTheReportPartial(t *testing.T) {
	f := newFixture(t)
	f.fork()
	// Same schema and row count, different content: only the content digest can tell.
	f.exec(f.base(), "BEGIN READ WRITE; UPDATE customers SET email = 'x@example.com' WHERE id = 1; COMMIT")
	report, err := f.diff()
	if code(err) != "review_partial" || !report.BaseChanged || !slices.Contains(report.BaseChangedItems, "public.customers") {
		t.Fatalf("diff after base change = %v, %+v", err, report.BaseChangedItems)
	}
	if report.Base.ForkFingerprint == report.Base.Fingerprint {
		t.Error("the report must print both fingerprints")
	}
}

func TestUnsupportedTablesAreListedNotSkipped(t *testing.T) {
	f := newFixture(t,
		`CREATE TABLE events (at timestamptz, what text)`,
		`CREATE TABLE windows (span interval PRIMARY KEY, label text)`,
		`CREATE FUNCTION double(i integer) RETURNS integer LANGUAGE sql AS 'SELECT i * 2'`)
	f.fork()
	f.exec(f.work(), `ALTER TABLE customers ADD COLUMN tier text;
		CREATE OR REPLACE FUNCTION double(i integer) RETURNS integer LANGUAGE sql AS 'SELECT i + i'`)
	report, err := f.diff()
	if code(err) != "review_partial" || report.Status != "partial" {
		t.Fatalf("diff = %v, status %s", err, report.Status)
	}
	reasons := map[string]string{}
	for _, u := range report.Unsupported {
		reasons[u.Table] = u.Reason
	}
	want := map[string]string{"events": "no_primary_key", "windows": "pk_type", "customers": "schema_changed"}
	for table, reason := range want {
		if reasons[table] != reason {
			t.Errorf("%s reason = %q; want %q", table, reasons[table], reason)
		}
	}
	if !slices.ContainsFunc(report.SchemaChanges, func(c SchemaChange) bool { return c.Kind == "function" && c.Change == "changed" }) {
		t.Errorf("schema_changes = %+v; want the replaced function", report.SchemaChanges)
	}
	if exit := tableReport(t, report, "orders"); exit.Status != "unchanged" {
		t.Error("other tables are still compared")
	}
}

func TestOverCapTablesAreCopiedSchemaOnly(t *testing.T) {
	f := newFixture(t, "ANALYZE")
	old := maxTableRows
	maxTableRows = 2.5 // customers, orders and order_items hold 3 rows each after ANALYZE
	t.Cleanup(func() { maxTableRows = old })
	result := f.fork()
	if tooLarge, _ := result["too_large"].([]string); len(tooLarge) != 3 {
		t.Fatalf("too_large = %v", result["too_large"])
	}
	if got := f.query(f.work(), "SELECT count(*)::text FROM customers"); got != "0" {
		t.Errorf("over-cap table data must not be copied; rows = %s", got)
	}
	report, err := f.diff()
	if code(err) != "review_partial" || len(report.Unsupported) != 3 || report.Unsupported[0].Reason != "too_large" || report.BaseChanged {
		t.Fatalf("diff = %v %+v base_changed=%v", err, report.Unsupported, report.BaseChanged)
	}
}

func TestChecksRunInOneReadOnlySnapshot(t *testing.T) {
	f := newFixture(t)
	f.fork()
	f.exec(f.work(), "UPDATE customers SET country = 'TR' WHERE country IS NULL")
	if _, err := f.diff(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "checks.sql")
	_ = os.WriteFile(file, []byte(`-- name: every customer has a country
SELECT count(*) = 0 FROM customers WHERE country IS NULL

-- name: wrong on purpose
SELECT count(*) = 99 FROM orders

-- name: writes are refused
UPDATE orders SET total = 0 RETURNING true

SELECT 1;
SELECT true
`), 0o600)
	cmd := f.cmd("check")
	cmd.File = file
	result, err := Run(context.Background(), cmd, nil)
	if code(err) != "check_failed" {
		t.Fatalf("check = %v", err)
	}
	results := result.(map[string]any)["checks"].([]CheckResult)
	got := make([]string, len(results))
	for i, r := range results {
		got[i] = r.Name + "=" + r.Result
	}
	want := []string{"every customer has a country=pass", "wrong on purpose=fail", "writes are refused=error", "4=error"}
	if !slices.Equal(got, want) {
		t.Fatalf("results = %v; want %v", got, want)
	}
	if !strings.Contains(results[2].Detail, "25006") || !strings.Contains(results[3].Detail, "blank line") {
		t.Errorf("details = %q / %q", results[2].Detail, results[3].Detail)
	}
	if got := f.query(f.work(), "SELECT sum(total)::text FROM orders"); got == "0.00" {
		t.Fatal("a check wrote to the work copy")
	}
	md, _ := os.ReadFile(filepath.Join(f.dir(), "report.md"))
	if !strings.Contains(string(md), "on the same work state this report compares") {
		t.Errorf("report.md must say the checks saw the compared state:\n%s", md)
	}
	var checks Checks
	if err := readJSON(filepath.Join(f.dir(), "checks.json"), &checks); err != nil || checks.Schema != 1 || checks.WorkFingerprint == "" {
		t.Fatalf("checks.json = %+v, %v", checks, err)
	}
	if _, err := readGeneration(f.dir()); err != nil {
		t.Errorf("check left an incomplete generation: %v", err)
	}
	// A new diff starts a new generation and drops the old checks.
	if _, err := f.diff(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.dir(), "checks.json")); !os.IsNotExist(err) {
		t.Error("diff must remove checks.json from the previous generation")
	}
}

func TestExportCopiesOneCompleteGeneration(t *testing.T) {
	f := newFixture(t)
	f.fork()
	cmd := f.cmd("export")
	cmd.Output = filepath.Join(t.TempDir(), "out")
	if _, err := Run(context.Background(), cmd, nil); code(err) != "invalid_input" {
		t.Fatalf("export before diff = %v", err)
	}
	if _, err := f.diff(); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), cmd, nil); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(cmd.Output)
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	if !slices.Equal(names, []string{"changes.sql", "manifest.json", "report.json", "report.md"}) {
		t.Errorf("exported %v", names)
	}
	if _, err := Run(context.Background(), cmd, nil); code(err) != "invalid_input" {
		t.Error("export must refuse an existing directory")
	}
	_ = os.WriteFile(filepath.Join(f.dir(), "changes.sql"), []byte("tampered"), 0o600)
	cmd.Output += "2"
	if _, err := Run(context.Background(), cmd, nil); code(err) != "invalid_input" {
		t.Error("export must refuse an incomplete generation")
	}
}

func TestDropRemovesOnlyMarkedCopies(t *testing.T) {
	f := newFixture(t)
	if _, err := Run(context.Background(), f.cmd("drop"), nil); code(err) != "invalid_input" {
		t.Fatalf("drop without --to or a target file = %v", err)
	}
	f.exec(f.admin, "CREATE DATABASE "+quoteIdent(f.name+"_base"))
	cmd := f.cmd("drop")
	cmd.To = f.admin
	if _, err := Run(context.Background(), cmd, nil); code(err) != "invalid_input" || !strings.Contains(err.Error(), "DROP DATABASE") {
		t.Fatalf("drop of an unmarked database = %v", err)
	}
	f.exec(f.admin, "DROP DATABASE "+quoteIdent(f.name+"_base"))
	f.fork()
	if _, err := Run(context.Background(), f.cmd("drop"), nil); err != nil {
		t.Fatalf("drop with the target file: %v", err)
	}
	if got := f.query(f.admin, "SELECT count(*)::text FROM pg_database WHERE datname IN ('"+f.name+"_base', '"+f.name+"_work')"); got != "0" {
		t.Errorf("copies left: %s", got)
	}
	if _, err := os.Stat(filepath.Join(f.dir(), "target")); !os.IsNotExist(err) {
		t.Error("drop must retire the target file")
	}
}

func TestDSNModeComparesTwoNamedDatabases(t *testing.T) {
	f := newFixture(t)
	f.fork()
	f.exec(f.work(), "DELETE FROM order_items WHERE order_id = 1")
	cmd := f.cmd("diff")
	cmd.Name = f.name + "_dsn"
	cmd.Base, cmd.Work = f.base(), f.work()
	if _, err := Run(context.Background(), cmd, nil); err != nil {
		t.Fatal(err)
	}
	var report Report
	if err := readJSON(filepath.Join(f.root, cmd.Name, "report.json"), &report); err != nil || report.Mode != "dsn" || report.Totals.Deleted != 1 {
		t.Fatalf("DSN-mode report = %+v, %v", report, err)
	}
	cmd.To = f.admin
	if _, err := Run(context.Background(), cmd, nil); code(err) != "invalid_input" {
		t.Error("--to with --base/--work must be refused")
	}
}

// Applying changes.sql to a copy of base must reproduce work, type by type.
func TestChangesSQLRoundTrip(t *testing.T) {
	f := newFixture(t, `CREATE TYPE mood AS ENUM ('calm', 'busy');
		CREATE DOMAIN positive AS integer CHECK (VALUE > 0);
		CREATE TABLE kinds (
			id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			at timestamptz, amount numeric(12, 4), ratio float8, blob bytea, doc jsonb,
			tags text[], feeling mood, size positive, note text,
			note_length integer GENERATED ALWAYS AS (length(note)) STORED);
		INSERT INTO kinds (at, amount, ratio, blob, doc, tags, feeling, size, note) VALUES
			('2026-01-02 03:04:05.678+03', 1.2345, 0.1, '\x00ff', '{"a": [1, "x"]}', '{a,"b c"}', 'calm', 1, 'it''s'),
			(NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL)`)
	f.fork()
	f.exec(f.work(), `UPDATE kinds SET at = '-infinity', amount = -0.0001, ratio = 1e300, blob = '\x5c00', doc = '{"q": "\"x\""}',
			tags = '{}', feeling = 'busy', size = 7, note = E'line\nbreak\\' WHERE id = 1;
		UPDATE kinds SET note = 'was null' WHERE id = 2;
		INSERT INTO kinds (at, note) VALUES ('2026-11-10', 'new');
		UPDATE customers SET country = 'TR' WHERE country IS NULL;
		DELETE FROM order_items WHERE order_id = 3;
		INSERT INTO orders VALUES (4, 1, 'pending', 3.00)`)
	report, err := f.diff()
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	f.exec(f.admin, "CREATE DATABASE "+quoteIdent(f.name+"_rt")+" TEMPLATE "+quoteIdent(f.name+"_base"))
	rt := withDatabase(f.admin, f.name+"_rt")
	sql, err := os.ReadFile(filepath.Join(f.dir(), "changes.sql"))
	if err != nil {
		t.Fatal(err)
	}
	f.exec(rt, "SET default_transaction_read_only = off;\n"+string(sql))
	ctx := context.Background()
	conn, err := connect(ctx, rt, "")
	if err != nil {
		t.Fatal(err)
	}
	defer closeConn(ctx, conn)
	tx, err := snapshotTx(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(ctx, tx)
	got, err := workFingerprint(ctx, tx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != report.Work.Fingerprint {
		t.Fatalf("changes.sql applied to base gives fingerprint %s; work is %s\n%s", got, report.Work.Fingerprint, sql)
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
