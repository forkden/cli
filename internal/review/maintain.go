package review

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// export copies one complete generation into a new directory and nothing else.
func (r *runner) export() (any, error) {
	if r.cmd.Output == "" {
		return nil, newError("invalid_input", "export needs --output", "pass a directory that does not exist yet", "nothing was written", "")
	}
	unlock, err := r.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	gen, err := readGeneration(r.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, newError("invalid_input", "there is no report in "+r.dir+" to export", "run forkden review diff "+r.cmd.Name+" first", "nothing was written", "")
	}
	if err != nil {
		return nil, r.incomplete()
	}
	if err := os.Mkdir(r.cmd.Output, 0o700); err != nil {
		return nil, newError("invalid_input", "cannot create "+r.cmd.Output+": "+err.Error(),
			"choose a new directory; export never writes into an existing one", "nothing was written", "")
	}
	names := []string{"manifest.json"}
	for name := range gen.Files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := copyFile(filepath.Join(r.dir, name), filepath.Join(r.cmd.Output, name)); err != nil {
			_ = os.RemoveAll(r.cmd.Output)
			return nil, r.writeFailed(r.cmd.Output, err)
		}
	}
	r.logf("export: %d files in %s; keep them out of synced or shared folders, they hold row values", len(names), r.cmd.Output)
	return map[string]any{"name": r.cmd.Name, "output": r.cmd.Output, "files": names, "generated_at": gen.GeneratedAt}, nil
}

func copyFile(from, to string) error {
	//nolint:gosec // from is a fixed artifact name inside the review directory.
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	//nolint:gosec // to is inside the new export directory.
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	return errors.Join(copyErr, out.Close())
}

// drop removes both copies; it refuses any database without this review's marker.
func (r *runner) drop(ctx context.Context) (any, error) {
	target, err := r.resolveTarget(true)
	if err != nil {
		return nil, err
	}
	unlock, err := r.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	cfg, err := parseDSN(target)
	if err != nil {
		return nil, newError("invalid_input", err.Error(), "check --to", "nothing was dropped", "")
	}
	conn, err := connect(ctx, target, "")
	if err != nil {
		return nil, r.traceError("target_unreachable", "cannot connect to the target server "+display(cfg), "check --to, PGPASSWORD or ~/.pgpass", "nothing was dropped", err)
	}
	defer closeConn(ctx, conn)
	existing, err := existingDatabases(ctx, conn, r.base(), r.work())
	if err != nil {
		return nil, r.traceError("drop_failed", "cannot list databases on the target server", "", "nothing was dropped", err)
	}
	if err := r.dropMarked(ctx, conn, existing); err != nil {
		return nil, err
	}
	for _, name := range []string{"target", "base.fingerprint"} {
		if err := os.Remove(filepath.Join(r.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, r.writeFailed(r.dir, err)
		}
	}
	r.logf("drop: removed %s; reports in %s are kept", strings.Join(append([]string{}, existing...), " and "), r.dir)
	return map[string]any{"name": r.cmd.Name, "dropped": existing, "reports": r.dir}, nil
}

func (r *runner) dropMarked(ctx context.Context, conn *pgx.Conn, names []string) error {
	manual := make([]string, len(names))
	for i, name := range names {
		manual[i] = "DROP DATABASE " + quoteIdent(name) + ";"
	}
	for _, name := range names {
		var comment *string
		if err := conn.QueryRow(ctx, "SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1", name).Scan(&comment); err != nil {
			return r.traceError("drop_failed", "cannot read the marker on "+name, "", "nothing was dropped", err)
		}
		if comment == nil || *comment != r.marker() {
			return newError("invalid_input", name+" does not carry the marker '"+r.marker()+"', so review will not drop it",
				"if you are sure it is a copy you made, drop it yourself: "+strings.Join(manual, " "), "nothing was dropped",
				"the marker is a database comment; any database owner can set it")
		}
	}
	for _, name := range names {
		if _, err := conn.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", name); err != nil {
			return newError("invalid_input", "cannot end the other sessions on "+name+": "+publicCause(err),
				"close the sessions yourself, then run: "+strings.Join(manual, " "), "nothing more was dropped", "")
		}
		if _, err := conn.Exec(ctx, "DROP DATABASE "+quoteIdent(name)); err != nil {
			return r.traceError("drop_failed", "DROP DATABASE "+name+" failed", "run: "+strings.Join(manual, " "), "databases dropped before this one stay dropped", err)
		}
	}
	return nil
}
