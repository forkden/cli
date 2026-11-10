package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	v1 "github.com/forkden/cli/api/v1"
)

type checkStatement struct {
	Name string
	SQL  string
}

// parseChecks splits a check file on blank lines. A statement whose first line is
// "-- name: LABEL" takes that label; a trailing ";" is dropped.
func parseChecks(text string) ([]checkStatement, error) {
	var statements []checkStatement
	var block []string
	flush := func() error {
		if len(block) == 0 {
			return nil
		}
		name := ""
		if first := strings.TrimSpace(block[0]); strings.HasPrefix(first, "-- name:") {
			name = strings.TrimSpace(strings.TrimPrefix(first, "-- name:"))
			block = block[1:]
		}
		sql := strings.TrimSpace(strings.Join(block, "\n"))
		sql = strings.TrimSpace(strings.TrimSuffix(sql, ";"))
		block = nil
		if sql == "" || onlyComments(sql) {
			if name != "" {
				return fmt.Errorf("check %q has no SQL", name)
			}
			return nil
		}
		if name == "" {
			name = strconv.Itoa(len(statements) + 1)
		}
		statements = append(statements, checkStatement{Name: name, SQL: sql})
		return nil
	}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		block = append(block, line)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(statements) == 0 {
		return nil, errors.New("the check file holds no statements")
	}
	return statements, nil
}

func onlyComments(sql string) bool {
	for _, line := range strings.Split(sql, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "--") {
			return false
		}
	}
	return true
}

// joinedStatements spots two statements joined by ";" without the blank line.
var joinedStatements = regexp.MustCompile(`;\s*\n\s*\S`)

func readCheckFile(path string) (string, error) {
	//nolint:gosec // Reading the explicitly selected local check file is the CLI operation.
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, v1.MaxStatementBytes+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return "", err
	}
	if len(data) > v1.MaxStatementBytes {
		return "", fmt.Errorf("the check file is larger than %d bytes", v1.MaxStatementBytes)
	}
	return string(data), nil
}

func (r *runner) check(ctx context.Context) (any, error) {
	if r.cmd.File == "" {
		return nil, newError("invalid_input", "check needs --file", "write one boolean query per block, blocks separated by a blank line", "nothing was run", "")
	}
	text, err := readCheckFile(r.cmd.File)
	if err != nil {
		return nil, newError("invalid_input", "cannot read "+r.cmd.File+": "+err.Error(), "", "nothing was run", "")
	}
	statements, err := parseChecks(text)
	if err != nil {
		return nil, newError("invalid_input", err.Error(), "separate statements with a blank line; start a block with '-- name: LABEL' to name it", "nothing was run", "")
	}
	workDSN := r.cmd.Work
	if workDSN == "" {
		if r.cmd.Base != "" {
			return nil, newError("invalid_input", "check runs on the work database only; --base is not used", "pass --work", "nothing was run", "")
		}
		target, err := r.resolveTarget(false)
		if err != nil {
			return nil, err
		}
		workDSN = withDatabase(target, r.work())
	} else if r.cmd.To != "" {
		return nil, newError("invalid_input", "--to cannot be combined with --work", "", "nothing was run", "")
	}
	unlock, err := r.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	var report *Report
	if _, err := os.Stat(filepath.Join(r.dir, "report.json")); err == nil {
		if _, err := readGeneration(r.dir); err != nil {
			return nil, r.incomplete()
		}
		report = &Report{}
		if err := readJSON(filepath.Join(r.dir, "report.json"), report); err != nil || report.Schema != 1 {
			return nil, newError("invalid_input", "report.json in "+r.dir+" is not a schema 1 report", "rerun forkden review diff "+r.cmd.Name, "nothing was run", "")
		}
	}
	conn, err := connect(ctx, workDSN, "")
	if err != nil {
		if missingDatabase(err) {
			return nil, newError("invalid_input", "the work database does not exist", "run forkden review fork "+r.cmd.Name+" first", "nothing was run", "")
		}
		return nil, r.traceError("target_unreachable", "cannot connect to the work database", "check --to or the target file", "nothing was run", err)
	}
	defer closeConn(ctx, conn)
	target := display(conn.Config())
	if report != nil && report.Work.DSN != target {
		return nil, newError("invalid_input", "report.json describes "+report.Work.DSN+", but check runs on "+target,
			"rerun forkden review diff "+r.cmd.Name+" against the same database", "nothing was run", "")
	}
	tx, err := snapshotTx(ctx, conn)
	if err != nil {
		return nil, r.traceError("target_unreachable", "cannot open a read-only snapshot on the work database", "", "nothing was run", err)
	}
	defer rollback(ctx, tx)
	// Checks are the user's SQL: they resolve names with the database's own search_path.
	if _, err := tx.Exec(ctx, "SET LOCAL search_path TO DEFAULT"); err != nil {
		return nil, r.traceError("target_unreachable", "cannot prepare the check session", "", "nothing was run", err)
	}
	checks := &Checks{Schema: 1, Name: r.cmd.Name, Target: target, GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	failed := false
	for _, statement := range statements {
		result := runCheck(ctx, tx, statement)
		failed = failed || result.Result != "pass"
		checks.Results = append(checks.Results, result)
		line := fmt.Sprintf("  %-5s %s", result.Result, statement.Name)
		if result.Detail != "" {
			line += " — " + strings.ReplaceAll(result.Detail, "\n", " ")
		}
		r.logf("%s", line)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// The same snapshot gives the work fingerprint, so report.md can tell whether the
	// checks saw the state the report compares.
	tooLarge := map[string]bool{}
	if report != nil {
		for _, u := range report.Unsupported {
			if u.Reason == "too_large" {
				tooLarge[u.Schema+"."+u.Table] = true
			}
		}
	}
	// A check may have changed session settings; the fingerprint needs the canonical ones.
	err = canonicalSettings(ctx, tx)
	if err == nil {
		checks.WorkFingerprint, err = workFingerprint(ctx, tx, tooLarge)
	}
	if err != nil {
		return nil, r.traceError("check_failed", "the checks ran, but the work fingerprint could not be computed", "rerun the check", "no checks.json was written", err)
	}
	if err := r.writeChecks(checks, report); err != nil {
		return nil, err
	}
	result := map[string]any{"name": r.cmd.Name, "checks": checks.Results, "path": filepath.Join(r.dir, "checks.json")}
	if failed {
		return result, newError("check_failed", "at least one check did not pass", "read the failing check's result above", "checks.json was written; nothing was changed", "")
	}
	return result, nil
}

func runCheck(ctx context.Context, tx pgx.Tx, statement checkStatement) CheckResult {
	result := CheckResult{Name: statement.Name}
	if _, err := tx.Exec(ctx, "SAVEPOINT forkden_check"); err != nil {
		result.Result, result.Detail = "error", publicCause(err)
		return result
	}
	value, err := evaluate(ctx, tx, statement.SQL)
	switch {
	case err != nil:
		_, _ = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT forkden_check")
		result.Result, result.Detail = "error", publicCause(err)
		if joinedStatements.MatchString(statement.SQL) {
			result.Detail += "\nhint: separate statements with a blank line; one block is one statement"
		}
	case value:
		result.Result = "pass"
	default:
		result.Result = "fail"
	}
	_, _ = tx.Exec(ctx, "RELEASE SAVEPOINT forkden_check")
	return result
}

func evaluate(ctx context.Context, tx pgx.Tx, sql string) (bool, error) {
	// Describe first: the extended protocol refuses two statements in one block,
	// which the grammar hint explains.
	rows, err := tx.Query(ctx, sql, pgx.QueryExecModeDescribeExec)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	fields := rows.FieldDescriptions()
	boolean := len(fields) == 1 && fields[0].DataTypeOID == 16
	var values []*bool
	for boolean && rows.Next() {
		var value *bool
		if err := rows.Scan(&value); err != nil {
			return false, err
		}
		values = append(values, value)
	}
	rows.Close()
	// A server error (a write in the read-only snapshot, a timeout) explains more than the shape.
	if err := rows.Err(); err != nil {
		return false, err
	}
	if !boolean {
		return false, errors.New("a check must return exactly one boolean column")
	}
	if len(values) != 1 {
		return false, fmt.Errorf("a check must return exactly one row, got %d", len(values))
	}
	if values[0] == nil {
		return false, errors.New("the check returned NULL")
	}
	return *values[0], nil
}

func workFingerprint(ctx context.Context, tx pgx.Tx, tooLarge map[string]bool) (string, error) {
	manifest, err := readManifest(ctx, tx)
	if err != nil {
		return "", err
	}
	tables, err := listTables(ctx, tx)
	if err != nil {
		return "", err
	}
	for _, t := range tables {
		if tooLarge[t.key()] {
			t.TooLarge = true
			continue
		}
		if err := digestTable(ctx, tx, t); err != nil {
			return "", err
		}
	}
	return fingerprint(manifestDigest(manifest), tables), nil
}

func (r *runner) writeChecks(checks *Checks, report *Report) error {
	stage, err := newStage(r.dir)
	if err != nil {
		return r.writeFailed(r.dir, err)
	}
	data, err := json.MarshalIndent(checks, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(stage, "checks.json"), append(data, '\n'), 0o600)
	}
	keep := []string{}
	generatedAt := checks.GeneratedAt
	if report != nil && err == nil {
		err = os.WriteFile(filepath.Join(stage, "report.md"), renderMarkdown(report, checks), 0o600)
		keep = []string{"report.json", "changes.sql"}
		generatedAt = report.GeneratedAt
	}
	if err == nil {
		err = commitStage(r.dir, stage, generatedAt, keep, nil)
	}
	if err != nil {
		_ = os.RemoveAll(stage)
		return r.writeFailed(r.dir, err)
	}
	return nil
}
