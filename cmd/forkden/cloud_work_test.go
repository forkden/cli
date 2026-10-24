package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forkden/cli/api/cloud"
	"github.com/forkden/cli/internal/account"
)

func TestCloudWorkScopeAndPrivateExport(t *testing.T) {
	session := cloud.WorkSession{ID: "wss_" + strings.Repeat("a", 32), OrgID: "org_demo", ProjectID: "prj_demo", ResourceID: "res_demo", ExpiresAt: time.Now().Add(10 * time.Minute)}
	token := "fws_" + strings.Repeat("b", 43)
	var executions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" {
			t.Error("work mixed account/browser authority or omitted its fork bearer")
		}
		var data any = session
		if r.URL.Path == "/v1/work/execute" {
			executions.Add(1)
			var input cloud.WorkInput
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if input.Kind == "export" {
				data = cloud.WorkResult{Kind: "export", Status: "succeeded", SQL: "UPDATE public.users SET country='TR' WHERE id=1;\n", Report: json.RawMessage(`{"data_coverage_complete":false,"diff":[]}`)}
			} else {
				data = cloud.WorkResult{Kind: input.Kind, Status: "failed", ErrorCode: "sql_failed", Operation: json.RawMessage(`{"transaction_status":"rolled_back","affected_rows":null}`)}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	credentials := &memoryCredentials{values: map[string]account.Credential{session.ID: {APIURL: server.URL, Token: token}}}
	app := cloudApp{workCredentials: credentials}
	cfg := account.Config{APIURL: server.URL, OrgID: session.OrgID, ProjectID: session.ProjectID, WorkSessionID: session.ID, WorkResourceID: session.ResourceID, WorkCredentialID: session.ID}
	bad := cfg
	bad.ProjectID = "prj_other"
	if _, err := app.workClient(t.Context(), bad); err == nil || executions.Load() != 0 {
		t.Fatal("mismatched project reached execution")
	}
	bad = cfg
	bad.APIURL = "https://other.example"
	if _, err := app.workClient(t.Context(), bad); err == nil {
		t.Fatal("work credential followed an origin override")
	}
	destination := filepath.Join(t.TempDir(), "export")
	var progress bytes.Buffer
	if _, err := app.work(t.Context(), "export", []string{"--output_dir", destination}, cfg, io.Discard, &progress); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"changes.sql", "report.json"} {
		info, err := os.Stat(filepath.Join(destination, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("export file missing or shared permissions")
		}
	}
	//nolint:gosec // File is in this test's freshly created private export directory.
	report, err := os.ReadFile(filepath.Join(destination, "report.json"))
	if err != nil || !bytes.Contains(report, []byte(`"data_coverage_complete": false`)) {
		t.Fatal("export hid incomplete coverage")
	}
	if _, err := app.work(t.Context(), "export", []string{"--output_dir", destination}, cfg, io.Discard, &progress); err == nil {
		t.Fatal("existing export destination overwritten")
	}
	result, err := app.work(t.Context(), "query", []string{"UPDATE public.users SET country='TR'"}, cfg, io.Discard, &progress)
	if err == nil || result.(cloud.WorkResult).Status != "failed" {
		t.Fatal("failed transaction lost classification or exited successfully")
	}
	if strings.Contains(progress.String(), token) || strings.Contains(progress.String(), "UPDATE") {
		t.Fatal("credential or SQL appeared in progress")
	}
}

func TestCloudWorkLostResponseIsNeverReplayed(t *testing.T) {
	session := cloud.WorkSession{ID: "wss_" + strings.Repeat("a", 32), OrgID: "org_demo", ProjectID: "prj_demo", ResourceID: "res_demo", ExpiresAt: time.Now().Add(time.Minute)}
	token := "fws_" + strings.Repeat("b", 43)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/work/session" {
			if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": session}); err != nil {
				t.Error(err)
			}
			return
		}
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	app := cloudApp{workCredentials: &memoryCredentials{values: map[string]account.Credential{session.ID: {APIURL: server.URL, Token: token}}}}
	cfg := account.Config{APIURL: server.URL, OrgID: session.OrgID, ProjectID: session.ProjectID, WorkSessionID: session.ID, WorkResourceID: session.ResourceID, WorkCredentialID: session.ID}
	if _, err := app.work(t.Context(), "query", []string{"UPDATE public.users SET country='TR'"}, cfg, io.Discard, io.Discard); err == nil {
		t.Fatal("lost response was reported as success")
	}
	if attempts.Load() != 1 {
		t.Fatal("SQL request was automatically replayed")
	}
}
