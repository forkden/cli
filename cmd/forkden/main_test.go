package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/forkden/cli/api/v1"
)

type fakeEngine struct {
	response v1.Response
	err      error
	calls    int
}

func (f *fakeEngine) Execute(context.Context, v1.Action, any) (v1.Response, error) {
	f.calls++
	return f.response, f.err
}

func (f *fakeEngine) Status(context.Context) (v1.Response, error) { return f.response, f.err }

func TestHelpAndVersionWorkWithoutEngine(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"--socket", "/missing/engine.sock", "help"}, {"--socket", "/missing/engine.sock", "db", "help"}, {"--json", "version"}} {
		var out bytes.Buffer
		if err := run(t.Context(), args, &out); err != nil || out.Len() == 0 {
			t.Errorf("run(%v) = %q, %v; want offline help/version", args, out.String(), err)
		}
	}
}

func TestJSONInputErrorsRemainParseable(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"--json", "--unknown"}, {"--json", "query", "--fork", "fd_test", "--unknown"}, {"--json", "db", "remove", "sample", "extra"}} {
		var out bytes.Buffer
		err := run(t.Context(), args, &out)
		var envelope map[string]any
		if exitCode(err) != 2 || json.Unmarshal(out.Bytes(), &envelope) != nil || envelope["ok"] != false {
			t.Errorf("run(%v) = %q, %v; want JSON input error with exit 2", args, out.Bytes(), err)
		}
	}
}

func TestFalseCheckKeepsAuditDataAndExitCode(t *testing.T) {
	t.Parallel()
	err := &v1.Error{Code: "check_failed", Message: "SQL check returned false"}
	client := &fakeEngine{response: v1.Response{Data: json.RawMessage(`{"check_state":"failed","transaction_status":"committed"}`)}, err: err}
	var out bytes.Buffer
	value, operationErr := dispatch(t.Context(), client, []string{"check", "--fork", "fd_sample", "--sql", "SELECT false"}, &out)
	if writeErr := writeOutput(&out, value, operationErr, true); writeErr != nil {
		t.Fatal(writeErr)
	}
	if exitCode(operationErr) != 3 || client.calls != 1 || !bytes.Contains(out.Bytes(), []byte(`"check_state":"failed"`)) || !bytes.Contains(out.Bytes(), []byte(`"ok":false`)) {
		t.Fatalf("false check = %q, exit %d, calls %d; want retained audit and no retry", out.Bytes(), exitCode(operationErr), client.calls)
	}
}

func TestExportWritesOnClientAndPreservesExistingPaths(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "review")
	client := &fakeEngine{response: v1.Response{Data: json.RawMessage(`{"sql":"BEGIN;\nUPDATE users SET country='TR';\nCOMMIT;","report":{"fork":{"id":"fd_sample"}}}`)}}
	var out bytes.Buffer
	if _, err := dispatch(t.Context(), client, []string{"export", "--fork", "fd_sample", "--output", dir}, &out); err != nil {
		t.Fatal(err)
	}
	sqlPath := filepath.Join(dir, "changes.sql")
	//nolint:gosec // sqlPath is inside this test's private temporary directory.
	before, err := os.ReadFile(sqlPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(sqlPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("export file mode = %v, %v; want 0600", info, err)
	}
	if _, err := dispatch(t.Context(), client, []string{"export", "--fork", "fd_sample", "--output", dir}, &out); err == nil {
		t.Fatal("repeated export should refuse existing directory")
	}
	//nolint:gosec // sqlPath is inside this test's private temporary directory.
	after, err := os.ReadFile(sqlPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("existing artifact changed: %v", err)
	}
}

func TestInvalidTTLNeverReachesEngine(t *testing.T) {
	t.Parallel()
	for _, ttl := range []string{"0s", "1.5s", "25h", "9999999999h"} {
		client := &fakeEngine{}
		_, err := dispatch(t.Context(), client, []string{"fork", "create", "--source", "sample", "--ttl", ttl}, &bytes.Buffer{})
		if exitCode(err) != 2 || client.calls != 0 {
			t.Errorf("create ttl %q = %v, calls %d; want rejected before engine call", ttl, err, client.calls)
		}
	}
}
