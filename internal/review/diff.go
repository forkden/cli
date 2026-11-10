package review

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const maxSamples = 3

type side struct {
	dsn      string
	conn     *pgx.Conn
	tx       pgx.Tx
	manifest []manifestRecord
	tables   map[string]*table
}

func (r *runner) endpoints() (base, work, mode string, err error) {
	if r.cmd.Base != "" || r.cmd.Work != "" {
		if r.cmd.To != "" {
			return "", "", "", newError("invalid_input", "--to cannot be combined with --base/--work",
				"use --base and --work to compare two databases you name, or --to for this review's own copies", "nothing was changed", "")
		}
		if r.cmd.Base == "" || r.cmd.Work == "" {
			return "", "", "", newError("invalid_input", "DSN mode needs both --base and --work", "", "nothing was changed", "")
		}
		return r.cmd.Base, r.cmd.Work, "dsn", nil
	}
	target, err := r.resolveTarget(false)
	if err != nil {
		return "", "", "", err
	}
	return withDatabase(target, r.base()), withDatabase(target, r.work()), "fork", nil
}

func (r *runner) openSide(ctx context.Context, dsn, role string) (*side, error) {
	conn, err := connect(ctx, dsn, "")
	if err != nil {
		if missingDatabase(err) {
			return nil, newError("invalid_input", "the "+role+" database does not exist",
				"run forkden review fork "+r.cmd.Name+" --dsn SOURCE first (or pass --to for the server that holds it)", "nothing was changed", "")
		}
		return nil, r.traceError("target_unreachable", "cannot connect to the "+role+" database",
			"check --to (or the target file written by fork), PGPASSWORD or ~/.pgpass", "nothing was changed", err)
	}
	tx, err := snapshotTx(ctx, conn)
	if err != nil {
		closeConn(ctx, conn)
		return nil, r.traceError("diff_failed", "cannot open a read-only snapshot on the "+role+" database", "", "nothing was changed", err)
	}
	s := &side{dsn: display(conn.Config()), conn: conn, tx: tx, tables: map[string]*table{}}
	if s.manifest, err = readManifest(ctx, tx); err != nil {
		s.close(ctx)
		return nil, r.traceError("diff_failed", "cannot read the "+role+" schema", "", "nothing was changed", err)
	}
	tables, err := listTables(ctx, tx)
	if err != nil {
		s.close(ctx)
		return nil, r.traceError("diff_failed", "cannot read the "+role+" tables", "", "nothing was changed", err)
	}
	for _, t := range tables {
		s.tables[t.key()] = t
	}
	return s, nil
}

func (s *side) close(ctx context.Context) {
	rollback(ctx, s.tx)
	closeConn(ctx, s.conn)
}

func (r *runner) diff(ctx context.Context) (any, error) {
	start := time.Now()
	baseDSN, workDSN, mode, err := r.endpoints()
	if err != nil {
		return nil, err
	}
	unlock, err := r.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	var fork *forkRecord
	if mode == "fork" {
		var record forkRecord
		if err := readJSON(filepath.Join(r.dir, "base.fingerprint"), &record); err == nil && record.Schema == 1 {
			fork = &record
		} else if !errors.Is(err, os.ErrNotExist) && err != nil {
			return nil, newError("invalid_input", filepath.Join(r.dir, "base.fingerprint")+" is unreadable",
				"fork again with --force", "nothing was changed", "")
		}
	}
	// One REPEATABLE READ snapshot per database: writes committed after this point are excluded.
	base, err := r.openSide(ctx, baseDSN, "base")
	if err != nil {
		return nil, err
	}
	defer base.close(ctx)
	work, err := r.openSide(ctx, workDSN, "work")
	if err != nil {
		return nil, err
	}
	defer work.close(ctx)
	generatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	r.logf("diff: %s (base) vs %s (work)", base.dsn, work.dsn)

	stage, err := newStage(r.dir)
	if err != nil {
		return nil, r.writeFailed(r.dir, err)
	}
	changes, err := newChangeWriter(filepath.Join(stage, "changes.sql"), r.cmd.Name, generatedAt, base.dsn, work.dsn)
	if err != nil {
		_ = os.RemoveAll(stage)
		return nil, r.writeFailed(stage, err)
	}
	report := &Report{Schema: 1, Name: r.cmd.Name, Status: "ok", GeneratedAt: generatedAt, Mode: mode,
		Base: Endpoint{DSN: base.dsn}, Work: Endpoint{DSN: work.dsn},
		Tables: []TableReport{}, Unsupported: []Unsupported{}, SchemaChanges: []SchemaChange{}}
	if err := r.compare(ctx, base, work, fork, report, changes); err != nil {
		_ = changes.close()
		_ = os.RemoveAll(stage)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, r.traceError("diff_failed", "the comparison failed", "rerun forkden review diff "+r.cmd.Name,
			"no report was written; the previous report, if any, is unchanged", err)
	}
	if err := changes.close(); err != nil {
		_ = os.RemoveAll(stage)
		return nil, r.writeFailed(stage, err)
	}
	report.ChangesTruncated = changes.truncated
	if fork != nil {
		report.Timings.DumpMS, report.Timings.RestoreMS = fork.Timings.DumpMS, fork.Timings.RestoreMS
	}
	report.Timings.DiffMS = time.Since(start).Milliseconds()
	if len(report.Unsupported) > 0 || report.BaseChanged || report.ChangesTruncated {
		report.Status = "partial"
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(stage, "report.json"), append(data, '\n'), 0o600)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(stage, "report.md"), renderMarkdown(report, nil), 0o600)
	}
	// Checks describe an older work state; diff starts a new generation without them.
	if err == nil {
		err = commitStage(r.dir, stage, generatedAt, nil, []string{"checks.json"})
	}
	if err != nil {
		_ = os.RemoveAll(stage)
		return nil, r.writeFailed(r.dir, err)
	}
	return r.diffResult(report)
}

func (r *runner) diffResult(report *Report) (any, error) {
	paths := map[string]string{
		"report_md":   filepath.Join(r.dir, "report.md"),
		"report_json": filepath.Join(r.dir, "report.json"),
		"changes_sql": filepath.Join(r.dir, "changes.sql"),
	}
	for _, t := range report.Tables {
		if t.Status == "changed" {
			r.logf("  %s.%s: +%d inserted  ~%d updated  -%d deleted", t.Schema, t.Name, t.Inserted, t.Updated, t.Deleted)
		}
	}
	for _, u := range report.Unsupported {
		r.logf("  %s.%s: not compared (%s)", u.Schema, u.Table, u.Reason)
	}
	if report.BaseChanged {
		r.logf("  warning: the base copy changed after fork (%s)", strings.Join(report.BaseChangedItems, ", "))
	}
	r.logf("diff: %s, %d tables changed; report: %s", report.Status, report.Totals.TablesChanged, paths["report_md"])
	result := map[string]any{
		"name": report.Name, "status": report.Status, "generated_at": report.GeneratedAt,
		"totals": report.Totals, "unsupported": len(report.Unsupported), "base_changed": report.BaseChanged,
		"paths": paths, "timings": report.Timings,
	}
	if report.Status == "ok" {
		return result, nil
	}
	return result, newError("review_partial", "the report is partial: some tables were not compared or the base changed after fork",
		"read the 'Not compared' section of "+paths["report_md"], "the report files were written: "+paths["report_md"]+", "+paths["report_json"]+", "+paths["changes_sql"], "")
}

// compare classifies every table, scans the comparable ones and fills report.
func (r *runner) compare(ctx context.Context, base, work *side, fork *forkRecord, report *Report, changes *changeWriter) error {
	tooLarge := map[string]bool{}
	if fork != nil {
		for _, key := range fork.TooLarge {
			tooLarge[key] = true
		}
	}
	baseGroups, workGroups := groupByTable(base.manifest), groupByTable(work.manifest)
	keys := make([]string, 0, len(base.tables)+len(work.tables))
	for key := range base.tables {
		keys = append(keys, key)
	}
	for key := range work.tables {
		if base.tables[key] == nil {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		b, w := base.tables[key], work.tables[key]
		// Over-cap tables stay out of both fingerprints, whatever else is wrong with them.
		if tooLarge[key] || (b != nil && b.overCap()) || (w != nil && w.overCap()) {
			markTooLarge(b, w)
		}
		reason := ""
		switch {
		case b == nil || w == nil || !slices.Equal(baseGroups[key], workGroups[key]):
			reason = "schema_changed"
		case b.TooLarge:
			reason = "too_large"
		case len(b.PK) == 0:
			reason = "no_primary_key"
		case !b.pkComparable():
			reason = "pk_type"
		}
		if reason != "" {
			ref := b
			if ref == nil {
				ref = w
			}
			report.Unsupported = append(report.Unsupported, Unsupported{Schema: ref.Schema, Table: ref.Name, Reason: reason})
			if err := digestUnscanned(ctx, base, b); err != nil {
				return err
			}
			if err := digestUnscanned(ctx, work, w); err != nil {
				return err
			}
			continue
		}
		r.logf("  scanning %s", key)
		scan := &tableScan{table: b, base: base.tx, work: work.tx, changes: changes,
			result: TableReport{Schema: b.Schema, Name: b.Name, Status: "unchanged", Samples: []Sample{}}}
		if err := scan.run(ctx); err != nil {
			return fmt.Errorf("scan %s: %w", key, err)
		}
		b.Rows, b.SHA256, w.Rows, w.SHA256 = scan.baseRows, scan.baseHash, scan.workRows, scan.workHash
		if scan.overBudget {
			markTooLarge(b, w)
			report.Unsupported = append(report.Unsupported, Unsupported{Schema: b.Schema, Table: b.Name, Reason: "too_large"})
			continue
		}
		report.Timings.TablesScanned++
		res := scan.result
		if res.Inserted+res.Updated+res.Deleted > 0 {
			res.Status = "changed"
			report.Totals.TablesChanged++
			report.Totals.Inserted += res.Inserted
			report.Totals.Updated += res.Updated
			report.Totals.Deleted += res.Deleted
		}
		report.Tables = append(report.Tables, res)
	}
	report.SchemaChanges = objectChanges(base, work)
	baseManifest, workManifest := manifestDigest(base.manifest), manifestDigest(work.manifest)
	report.Base.Fingerprint = fingerprint(baseManifest, sortedTables(base.tables))
	report.Work.Fingerprint = fingerprint(workManifest, sortedTables(work.tables))
	if fork != nil {
		report.Base.ForkFingerprint = fork.Fingerprint
		if report.Base.Fingerprint != fork.Fingerprint {
			report.BaseChanged = true
			report.BaseChangedItems = baseChanges(fork, baseManifest, base.tables)
		}
	}
	return nil
}

func markTooLarge(tables ...*table) {
	for _, t := range tables {
		if t != nil {
			t.TooLarge = true
		}
	}
}

// digestUnscanned keeps tables that are not compared inside the fingerprint.
func digestUnscanned(ctx context.Context, s *side, t *table) error {
	if t == nil || t.TooLarge {
		return nil
	}
	return digestTable(ctx, s.tx, t)
}

func sortedTables(tables map[string]*table) []*table {
	list := make([]*table, 0, len(tables))
	for _, t := range tables {
		list = append(list, t)
	}
	sortTables(list)
	return list
}

func baseChanges(fork *forkRecord, manifest string, tables map[string]*table) []string {
	var items []string
	if manifest != fork.Manifest {
		items = append(items, "schema")
	}
	seen := map[string]bool{}
	for _, d := range fork.Tables {
		seen[d.Table] = true
		t := tables[d.Table]
		switch {
		case t == nil:
			items = append(items, d.Table+" (dropped)")
		case !t.TooLarge && (t.Rows != d.Rows || t.SHA256 != d.SHA256):
			items = append(items, d.Table)
		}
	}
	for key, t := range tables {
		if !seen[key] && !t.TooLarge && !slices.Contains(fork.TooLarge, key) {
			items = append(items, key+" (added)")
		}
	}
	slices.Sort(items)
	return items
}

func groupByTable(records []manifestRecord) map[string][]string {
	groups := map[string][]string{}
	for _, record := range records {
		if key := record.tableKey(); key != "" {
			groups[key] = append(groups[key], record.Kind+"\t"+record.Record)
		}
	}
	return groups
}

// objectChanges lists differences in records that do not belong to a compared table.
func objectChanges(base, work *side) []SchemaChange {
	group := func(s *side) map[[2]string][]string {
		groups := map[[2]string][]string{}
		for _, record := range s.manifest {
			if key := record.tableKey(); key != "" && (base.tables[key] != nil || work.tables[key] != nil) {
				continue
			}
			id := [2]string{record.Kind, recordIdentity(record)}
			groups[id] = append(groups[id], record.Record)
		}
		return groups
	}
	before, after := group(base), group(work)
	ids := make([][2]string, 0, len(before)+len(after))
	for id := range before {
		ids = append(ids, id)
	}
	for id := range after {
		if _, ok := before[id]; !ok {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b [2]string) int { return strings.Compare(a[0]+"\x00"+a[1], b[0]+"\x00"+b[1]) })
	changes := []SchemaChange{}
	for _, id := range ids {
		b, okB := before[id]
		a, okA := after[id]
		switch {
		case !okA:
			changes = append(changes, SchemaChange{Kind: id[0], Name: id[1], Change: "removed"})
		case !okB:
			changes = append(changes, SchemaChange{Kind: id[0], Name: id[1], Change: "added"})
		case !slices.Equal(a, b):
			changes = append(changes, SchemaChange{Kind: id[0], Name: id[1], Change: "changed"})
		}
	}
	return changes
}

// recordIdentity names the object a manifest record describes, without its definition.
func recordIdentity(record manifestRecord) string {
	var fields []any
	if err := json.Unmarshal([]byte(record.Record), &fields); err != nil || len(fields) == 0 {
		return record.Kind
	}
	text := func(i int) string {
		if i < len(fields) && fields[i] != nil {
			return fmt.Sprint(fields[i])
		}
		return ""
	}
	switch record.Kind {
	case "database":
		return "database"
	case "schema", "extension":
		return text(0)
	case "column":
		return text(0) + "." + text(1) + "." + text(2)
	case "constraint":
		owner := text(1)
		if owner == "" {
			owner = text(2)
		}
		return text(0) + "." + owner + "." + text(3)
	case "trigger":
		return text(0) + "." + text(1) + "." + text(2)
	case "function":
		return text(0) + "." + text(1) + "(" + text(2) + ")"
	}
	return text(0) + "." + text(1)
}

// tableScan walks base and work in primary-key order with two cursors.
//
//	base row only -> delete   work row only -> insert   both, text differs -> update
type tableScan struct {
	table      *table
	base, work pgx.Tx
	changes    *changeWriter
	result     TableReport

	baseRows, workRows int64
	baseHash, workHash string
	overBudget         bool
}

type scanRow struct {
	key  []any
	json []byte
	text []any // every column as text, for changes.sql
}

func (s *tableScan) run(ctx context.Context) error {
	baseCur := &cursor{tx: s.base, name: "forkden_review_base"}
	workCur := &cursor{tx: s.work, name: "forkden_review_work"}
	query := s.selectSQL()
	if err := baseCur.open(ctx, query); err != nil {
		return err
	}
	if err := workCur.open(ctx, query); err != nil {
		return err
	}
	baseDigest, workDigest := digestWriter{hash: sha256.New()}, digestWriter{hash: sha256.New()}
	b, err := baseCur.next(ctx, len(s.table.PK))
	if err != nil {
		return err
	}
	w, err := workCur.next(ctx, len(s.table.PK))
	if err != nil {
		return err
	}
	for b != nil || w != nil {
		if s.baseRows > maxScanRows || s.workRows > maxScanRows {
			s.overBudget = true
			break
		}
		order, err := classifyRow(b, w)
		if err != nil {
			return err
		}
		if err := s.recordChange(order, b, w); err != nil {
			return err
		}
		b, w, err = s.advanceCursors(ctx, order, b, w, baseCur, workCur, &baseDigest, &workDigest)
		if err != nil {
			return err
		}
	}
	if err := errors.Join(baseCur.close(ctx), workCur.close(ctx)); err != nil {
		return err
	}
	s.baseHash, s.workHash = hex.EncodeToString(baseDigest.hash.Sum(nil)), hex.EncodeToString(workDigest.hash.Sum(nil))
	return nil
}

func (s *tableScan) selectSQL() string {
	pk := make([]string, len(s.table.PK))
	for i, name := range s.table.PK {
		pk[i] = quoteIdent("t", name)
	}
	cols := make([]string, len(s.table.Columns))
	for i, c := range s.table.Columns {
		cols[i] = quoteIdent("t", c.Name) + "::text"
	}
	return "SELECT " + strings.Join(pk, ", ") + ", pg_catalog.row_to_json(t)::text, ARRAY[" + strings.Join(cols, ", ") + "]::text[] FROM ONLY " +
		quoteIdent(s.table.Schema, s.table.Name) + " AS t ORDER BY " + s.table.orderBy()
}

// classifyRow returns -1 when only base has the key (delete), 1 when only work has it
// (insert) and 0 when both do.
func classifyRow(b, w *scanRow) (int, error) {
	switch {
	case w == nil:
		return -1, nil
	case b == nil:
		return 1, nil
	}
	return compareKeys(b.key, w.key)
}

func (s *tableScan) recordChange(order int, b, w *scanRow) error {
	switch order {
	case -1:
		s.result.Deleted++
		s.sample("delete", b, nil)
		return s.changes.remove(s.table, b)
	case 1:
		s.result.Inserted++
		s.sample("insert", nil, w)
		return s.changes.insert(s.table, w)
	}
	if string(b.json) == string(w.json) {
		return nil
	}
	s.result.Updated++
	s.sample("update", b, w)
	return s.changes.update(s.table, b, w)
}

func (s *tableScan) advanceCursors(ctx context.Context, order int, b, w *scanRow, baseCur, workCur *cursor, baseDigest, workDigest *digestWriter) (*scanRow, *scanRow, error) {
	var err error
	if order <= 0 {
		baseDigest.writeCopyRow(b.json)
		s.baseRows++
		if b, err = baseCur.next(ctx, len(s.table.PK)); err != nil {
			return nil, nil, err
		}
	}
	if order >= 0 {
		workDigest.writeCopyRow(w.json)
		s.workRows++
		if w, err = workCur.next(ctx, len(s.table.PK)); err != nil {
			return nil, nil, err
		}
	}
	return b, w, nil
}

func (s *tableScan) sample(kind string, b, w *scanRow) {
	if len(s.result.Samples) >= maxSamples {
		return
	}
	sample := Sample{Kind: kind}
	if b != nil {
		sample.Old = json.RawMessage(b.json)
	}
	if w != nil {
		sample.New = json.RawMessage(w.json)
	}
	source := w
	if source == nil {
		source = b
	}
	sample.PK = pkObject(source.json, s.table.PK)
	s.result.Samples = append(s.result.Samples, sample)
}

// pkObject keeps the primary-key members of a row_to_json object, in key order.
func pkObject(row []byte, pk []string) json.RawMessage {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(row, &values); err != nil {
		return json.RawMessage("null")
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, name := range pk {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(name)
		b.Write(key)
		b.WriteByte(':')
		b.Write(values[name])
	}
	b.WriteByte('}')
	return json.RawMessage(b.String())
}

type cursor struct {
	tx     pgx.Tx
	name   string
	buffer []*scanRow
	done   bool
}

func (c *cursor) open(ctx context.Context, query string) error {
	_, err := c.tx.Exec(ctx, "DECLARE "+c.name+" NO SCROLL CURSOR WITHOUT HOLD FOR "+query)
	return err
}

func (c *cursor) close(ctx context.Context) error {
	_, err := c.tx.Exec(ctx, "CLOSE "+c.name)
	return err
}

func (c *cursor) next(ctx context.Context, keys int) (*scanRow, error) {
	if len(c.buffer) == 0 && !c.done {
		// The FETCH text repeats across tables of different shapes: describe it every time
		// instead of reusing a cached row description.
		rows, err := c.tx.Query(ctx, "FETCH FORWARD "+strconv.Itoa(fetchRows)+" FROM "+c.name, pgx.QueryExecModeDescribeExec)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			values, err := rows.Values()
			if err != nil {
				rows.Close()
				return nil, err
			}
			if len(values) != keys+2 {
				rows.Close()
				return nil, errors.New("scan returned an unexpected row shape")
			}
			text, _ := values[keys].(string)
			columns, _ := values[keys+1].([]any)
			c.buffer = append(c.buffer, &scanRow{key: values[:keys], json: []byte(text), text: columns})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(c.buffer) == 0 {
			c.done = true
		}
	}
	if len(c.buffer) == 0 {
		return nil, nil
	}
	row := c.buffer[0]
	c.buffer = c.buffer[1:]
	return row, nil
}

// changeWriter streams advisory SQL that reproduces the change on a copy of base.
type changeWriter struct {
	file      *os.File
	out       *bufio.Writer
	bytes     int64
	truncated bool
}

func newChangeWriter(path, name, generatedAt, base, work string) (*changeWriter, error) {
	//nolint:gosec // path is inside the review's private staging directory.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	c := &changeWriter{file: file, out: bufio.NewWriterSize(file, 1<<16)}
	c.write(fmt.Sprintf(`-- forkden review %s: row changes from %s (base) to %s (work)
-- generated %s
-- Advisory SQL for review. Statements follow each table's primary-key order, not
-- foreign-key order; apply it only to a copy, never to production as-is.
-- Generated columns are omitted; identity values use OVERRIDING SYSTEM VALUE.
SET standard_conforming_strings = on;
SET datestyle = 'ISO, YMD';
SET intervalstyle = 'postgres';
SET timezone = 'UTC';
BEGIN;
`, name, base, work, generatedAt))
	return c, nil
}

func (c *changeWriter) write(statement string) {
	if c.truncated {
		return
	}
	if c.bytes+int64(len(statement)) > maxChangeBytes {
		c.truncated = true
		_, _ = c.out.WriteString("-- changes.sql truncated at the size cap; report.json counts every change\n")
		return
	}
	n, _ := c.out.WriteString(statement)
	c.bytes += int64(n)
}

func (c *changeWriter) close() error {
	if !c.truncated {
		_, _ = c.out.WriteString("COMMIT;\n")
	}
	return errors.Join(c.out.Flush(), c.file.Close())
}

func (c *changeWriter) insert(t *table, row *scanRow) error {
	var names, values []string
	overriding := false
	for i, col := range t.Columns {
		if col.Generated {
			continue
		}
		names = append(names, quoteIdent(col.Name))
		values = append(values, literal(row.text, i, col.Type))
		overriding = overriding || col.Identity == "a"
	}
	statement := "INSERT INTO " + quoteIdent(t.Schema, t.Name) + " (" + strings.Join(names, ", ") + ")"
	if overriding {
		statement += " OVERRIDING SYSTEM VALUE"
	}
	c.write(statement + " VALUES (" + strings.Join(values, ", ") + ");\n")
	return nil
}

func (c *changeWriter) update(t *table, before, after *scanRow) error {
	var sets []string
	var notes []string
	for i, col := range t.Columns {
		if col.Generated || textAt(before.text, i) == textAt(after.text, i) {
			continue
		}
		if col.Identity == "a" {
			notes = append(notes, col.Name)
			continue
		}
		sets = append(sets, quoteIdent(col.Name)+" = "+literal(after.text, i, col.Type))
	}
	if len(notes) > 0 {
		c.write("-- identity column(s) " + strings.Join(notes, ", ") + " changed; GENERATED ALWAYS columns cannot be set by UPDATE\n")
	}
	if len(sets) == 0 {
		return nil
	}
	c.write("UPDATE " + quoteIdent(t.Schema, t.Name) + " SET " + strings.Join(sets, ", ") + " WHERE " + c.match(t, before) + ";\n")
	return nil
}

func (c *changeWriter) remove(t *table, row *scanRow) error {
	c.write("DELETE FROM " + quoteIdent(t.Schema, t.Name) + " WHERE " + c.match(t, row) + ";\n")
	return nil
}

func (c *changeWriter) match(t *table, row *scanRow) string {
	parts := make([]string, 0, len(t.PK))
	for _, name := range t.PK {
		i := slices.IndexFunc(t.Columns, func(col column) bool { return col.Name == name })
		if i < 0 {
			continue
		}
		parts = append(parts, quoteIdent(name)+" = "+literal(row.text, i, t.Columns[i].Type))
	}
	return strings.Join(parts, " AND ")
}

func textAt(values []any, i int) any {
	if i < len(values) {
		return values[i]
	}
	return nil
}

// literal is the one quoter changes.sql uses: a standard string cast to the column type.
func literal(values []any, i int, typ string) string {
	text, ok := textAt(values, i).(string)
	if !ok {
		return "NULL"
	}
	return quoteLiteral(text) + "::" + typ
}
