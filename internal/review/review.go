// Package review rehearses a data change on two local copies of a PostgreSQL
// database and writes a row-level report on the developer's machine.
//
//	fork   source --pg_dump (one exported snapshot)--> <name>_base (read-only) --TEMPLATE--> <name>_work
//	diff   <name>_base vs <name>_work, one REPEATABLE READ snapshot each --> report.json, report.md, changes.sql
//	check  boolean SQL checks on <name>_work in one snapshot --> checks.json (report.md re-rendered)
//	export copy one complete report generation into a new directory
//	drop   remove both copies (only databases carrying this review's marker)
//
// Nothing here writes to the source database, talks to Forkden's cloud or starts Docker.
package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	v1 "github.com/forkden/cli/api/v1"
)

// Command is one parsed `forkden review` invocation.
type Command struct {
	Action   string // fork, diff, check, export or drop
	Name     string
	DSN      string // fork: the source database
	To       string // target server; empty means the target file, then the libpq environment
	Base     string // diff DSN mode
	Work     string // diff/check DSN mode
	File     string // check file
	Output   string // export directory
	Force    bool
	Template bool
	// Root holds one directory per review name; empty means ./forkden-review.
	Root string
}

// Run executes one review command. Progress and the human summary go to progress;
// the returned value is the command's stdout data.
func Run(ctx context.Context, cmd Command, progress io.Writer) (any, error) {
	if progress == nil {
		progress = io.Discard
	}
	if !namePattern.MatchString(cmd.Name) {
		return nil, newError("invalid_input", "review name must match ^[a-z][a-z0-9_]{0,40}$ (it prefixes the two database names)",
			"choose a short lowercase name, for example: forkden review fork demo --dsn \"$SOURCE\"", "nothing was created", "")
	}
	if cmd.Root == "" {
		cmd.Root = "forkden-review"
	}
	r := &runner{cmd: cmd, progress: progress, dir: filepath.Join(cmd.Root, cmd.Name)}
	switch cmd.Action {
	case "fork":
		return r.fork(ctx)
	case "diff":
		return r.diff(ctx)
	case "check":
		return r.check(ctx)
	case "export":
		return r.export()
	case "drop":
		return r.drop(ctx)
	default:
		return nil, newError("invalid_input", "unknown review command "+cmd.Action, "run forkden help review", "nothing was changed", "")
	}
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

type runner struct {
	cmd      Command
	progress io.Writer
	dir      string
}

func (r *runner) base() string { return r.cmd.Name + "_base" }
func (r *runner) work() string { return r.cmd.Name + "_work" }
func (r *runner) marker() string {
	return "forkden-review:" + r.cmd.Name
}

func (r *runner) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(r.progress, format+"\n", args...)
}

// newError builds the fixed what / fix / recovery / context message layout.
func newError(code, what, fix, recovery, context string) *v1.Error {
	lines := []string{what}
	if fix != "" {
		lines = append(lines, "fix: "+fix)
	}
	if recovery != "" {
		lines = append(lines, "recovery: "+recovery)
	}
	if context != "" {
		lines = append(lines, "context: "+context)
	}
	return &v1.Error{Code: code, Message: strings.Join(lines, "\n")}
}

// traceError records the full cause in <dir>/<command>.log and returns the public error.
func (r *runner) traceError(code, what, fix, recovery string, cause error) *v1.Error {
	trace := r.writeTrace(cause)
	ctx := ""
	if cause != nil {
		ctx = scrub(publicCause(cause))
	}
	public := newError(code, what, fix, recovery, ctx)
	if trace != "" {
		public.Message += "\ntrace: " + trace
	}
	return public
}

func (r *runner) writeTrace(cause error) string {
	if cause == nil {
		return ""
	}
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return ""
	}
	path := filepath.Join(r.dir, r.cmd.Action+".log")
	text := time.Now().UTC().Format(time.RFC3339) + " " + r.cmd.Action + "\n" + scrub(fmt.Sprintf("%+v", cause)) + "\n"
	if err := appendFile(path, text); err != nil {
		return ""
	}
	return path
}

func appendFile(path, text string) error {
	//nolint:gosec // The trace path is inside the review's private directory.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = io.WriteString(file, text)
	return errors.Join(err, file.Close())
}

// publicCause keeps a PostgreSQL SQLSTATE and message; other errors keep their text.
func publicCause(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Sprintf("postgres %s: %s", pgErr.Code, pgErr.Message)
	}
	return err.Error()
}

var (
	urlPassword     = regexp.MustCompile(`(://[^:/@\s]*):[^@/\s]*@`)
	keywordPassword = regexp.MustCompile(`(?i)password\s*=\s*('(?:[^'\\]|\\.)*'|\S+)`)
)

// scrub removes passwords from DSNs embedded in free text (stderr tails, logs, messages).
func scrub(text string) string {
	text = urlPassword.ReplaceAllString(text, "$1@")
	return keywordPassword.ReplaceAllString(text, "password=REDACTED")
}

// redactDSN removes the password from a URL or keyword DSN and keeps every other option.
func redactDSN(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if isURL(dsn) {
		parsed, err := url.Parse(dsn)
		if err != nil {
			return scrub(dsn)
		}
		if parsed.User != nil {
			parsed.User = url.User(parsed.User.Username())
		}
		return parsed.String()
	}
	return strings.TrimSpace(keywordPassword.ReplaceAllString(dsn, ""))
}

func isURL(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// withDatabase returns dsn pointed at another database on the same server.
func withDatabase(dsn, database string) string {
	if isURL(dsn) {
		parsed, err := url.Parse(dsn)
		if err == nil {
			parsed.Path = "/" + database
			parsed.RawPath = ""
			return parsed.String()
		}
	}
	// libpq and pgx both let a later keyword override an earlier one.
	return strings.TrimSpace(dsn + " dbname=" + keywordValue(database))
}

func keywordValue(value string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(value) + "'"
}

// display renders a resolved connection as user@host:port/database, never with a password.
func display(cfg *pgx.ConnConfig) string {
	return fmt.Sprintf("%s@%s:%d/%s", cfg.User, cfg.Host, cfg.Port, cfg.Database)
}

func parseDSN(dsn string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL connection string: " + scrub(err.Error()))
	}
	return cfg, nil
}

func sameServer(a, b *pgx.ConnConfig) bool {
	return strings.EqualFold(a.Host, b.Host) && a.Port == b.Port
}

func connect(ctx context.Context, dsn, database string) (*pgx.Conn, error) {
	cfg, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}
	if database != "" {
		cfg.Database = database
	}
	cfg.ConnectTimeout = 10 * time.Second
	cfg.RuntimeParams["application_name"] = "forkden-review"
	return pgx.ConnectConfig(ctx, cfg)
}

func closeConn(ctx context.Context, conn *pgx.Conn) {
	if conn == nil {
		return
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = conn.Close(cleanup)
}

func missingDatabase(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "3D000"
}

// snapshotTx opens the canonical read-only snapshot every review read uses: rendered
// values must not depend on a user's session defaults.
func snapshotTx(ctx context.Context, conn *pgx.Conn) (pgx.Tx, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	if err := canonicalSettings(ctx, tx); err != nil {
		rollback(ctx, tx)
		return nil, err
	}
	return tx, nil
}

func canonicalSettings(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SET LOCAL search_path = pg_catalog;
		SET LOCAL timezone = 'UTC'; SET LOCAL datestyle = 'ISO, YMD';
		SET LOCAL intervalstyle = 'postgres'; SET LOCAL extra_float_digits = 3;
		SET LOCAL row_security = off; SET LOCAL work_mem = '16MB';
		SET LOCAL statement_timeout = '120s'; SET LOCAL lock_timeout = '5s'`)
	return err
}

func rollback(ctx context.Context, tx pgx.Tx) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(cleanup)
}

// lock serializes diff, check, export and drop on one review directory.
func (r *runner) lock() (func(), error) {
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return nil, r.writeFailed(r.dir, err)
	}
	file, err := os.OpenFile(filepath.Join(r.dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, r.writeFailed(r.dir, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, r.writeFailed(r.dir, err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func (r *runner) writeFailed(path string, err error) *v1.Error {
	return r.traceError("write_failed", "could not write review files under "+path,
		"check free disk space and that the directory is writable", "no partial report was left in place", err)
}

// resolveTarget returns the target server DSN: --to, then the fork's target file,
// then the libpq environment (an empty DSN).
func (r *runner) resolveTarget(requireExplicit bool) (string, error) {
	if r.cmd.To != "" {
		return r.cmd.To, nil
	}
	data, err := os.ReadFile(filepath.Join(r.dir, "target"))
	if err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", r.writeFailed(r.dir, err)
	}
	if requireExplicit {
		return "", newError("invalid_input", "no target server for review "+r.cmd.Name,
			"pass --to with the server that holds "+r.base()+" and "+r.work(),
			"nothing was changed", "the target file "+filepath.Join(r.dir, "target")+" is written by forkden review fork")
	}
	return "", nil
}

// toolPath finds a PostgreSQL client tool on PATH.
func toolPath(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", newError("tool_missing", name+" is not on PATH",
			"install the PostgreSQL client tools whose major version equals the target server's (macOS: brew install postgresql@17 and add its bin directory to PATH; Debian/Ubuntu: apt install postgresql-client-17)",
			"nothing was created", "forkden review runs pg_dump and pg_restore as subprocesses")
	}
	return path, nil
}

// runTool runs pg_dump or pg_restore without a prompt (-w); the password reaches the
// child only through PGPASSWORD. stderr is scrubbed and kept for the trace log.
func runTool(ctx context.Context, path string, args []string, password string) (string, error) {
	//nolint:gosec // Only pg_dump/pg_restore resolved from PATH; argv has no shell expansion.
	cmd := exec.CommandContext(ctx, path, append([]string{"-w"}, args...)...)
	env := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "PGPASSWORD=") {
			env = append(env, value)
		}
	}
	if password != "" {
		env = append(env, "PGPASSWORD="+password)
	}
	cmd.Env = env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	return scrub(stderr.String()), err
}

func tail(text string, lines int) string {
	parts := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func quoteIdent(parts ...string) string { return pgx.Identifier(parts).Sanitize() }

func toolOutput(ctx context.Context, path string, args ...string) (string, error) {
	//nolint:gosec // path is pg_dump or pg_restore resolved from PATH.
	out, err := exec.CommandContext(ctx, path, args...).Output()
	return string(out), err
}
