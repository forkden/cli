package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/forkden/cli/api/cloud"
	"github.com/forkden/cli/internal/account"
)

func TestCloudSQLInputModesPreserveStatementsAndRejectAmbiguity(t *testing.T) {
	t.Parallel()
	session := cloud.WorkSession{ID: "wss_" + strings.Repeat("a", 32), OrgID: "org_demo", ProjectID: "prj_demo", ResourceID: "res_demo", ExpiresAt: time.Now().Add(time.Minute)}
	token := "fws_" + strings.Repeat("b", 43)
	var mu sync.Mutex
	statements := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("work request omitted fork-scoped authorization")
		}
		var data any = session
		if r.URL.Path == "/v1/work/execute" {
			var input cloud.WorkInput
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			statements = append(statements, input.SQL)
			mu.Unlock()
			data = cloud.WorkResult{Kind: "query", Status: "succeeded", Operation: json.RawMessage(`{"transaction_status":"committed"}`)}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	credentials := &memoryCredentials{values: map[string]account.Credential{session.ID: {APIURL: server.URL, Token: token}}}
	app := cloudApp{workCredentials: credentials}
	cfg := account.Config{APIURL: server.URL, OrgID: session.OrgID, ProjectID: session.ProjectID, WorkSessionID: session.ID, WorkResourceID: session.ResourceID, WorkCredentialID: session.ID}
	sql := "-- keep --json --help unchanged\nSELECT 'hello';\n"
	file := filepath.Join(t.TempDir(), "statement.sql")
	if err := os.WriteFile(file, []byte(sql), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, input := range [][]string{{"--", sql}, {"--sql", sql}, {"--file", file}} {
		args := append([]string{"cloud", "query"}, input...)
		_, args, err := parseGlobalOptions(args)
		if err != nil {
			t.Fatal(err)
		}
		cmd, consumed, err := resolveCommand(args)
		if err != nil {
			t.Fatal(err)
		}
		args = orderCommandArgs(cmd, args, consumed)
		if _, err := app.work(t.Context(), "query", args[2:], cfg, io.Discard, io.Discard); err != nil {
			t.Errorf("cloud query(%v) = %v, want successful execution", input, err)
		}
	}
	for _, input := range [][]string{{"--sql", sql, sql}, {"--sql", sql, "--file", file}, {}, {"--unknown"}} {
		if _, err := app.work(t.Context(), "query", input, cfg, io.Discard, io.Discard); exitCode(err) != 2 {
			t.Errorf("cloud query(%v) exit = %d, want input failure before execution", input, exitCode(err))
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(statements) != 3 {
		t.Fatalf("cloud query input modes executed %d statements, want 3", len(statements))
	}
	for _, got := range statements {
		if got != sql {
			t.Errorf("cloud query statement = %q, want verbatim %q", got, sql)
		}
	}
}
