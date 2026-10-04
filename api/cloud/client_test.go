package cloud_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/forkden/cli/api/cloud"
)

func TestOriginAndTokenBoundary(t *testing.T) {
	t.Parallel()
	for _, origin := range []string{"http://example.com", "https://user:password@example.com", "https://example.com/path", "https://example.com?token=x", "https://example.com#x", "ftp://localhost", "//localhost", "https://example.com?"} {
		if _, err := cloud.NewClient(origin, ""); err == nil {
			t.Errorf("NewClient(%q) accepted unsafe origin", origin)
		}
	}
	for _, token := range []string{"fdr_" + strings.Repeat("A", 43), "fds_" + strings.Repeat("A", 43), "fda_short", "fda_" + strings.Repeat("%", 43)} {
		if _, err := cloud.NewClient("https://example.com", token); err == nil {
			t.Error("NewClient accepted a non-account token")
		}
	}
}

func TestRedirectNeverForwardsCredential(t *testing.T) {
	t.Parallel()
	var received atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { received.Add(1) }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, err := cloud.NewClient(server.URL, "fda_"+base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Session(t.Context()); err == nil {
		t.Fatal("redirect was accepted")
	}
	if received.Load() != 0 {
		t.Fatal("redirect destination received a request")
	}
}

func TestResponseProjectionFixedFailuresAndNoMutationRetry(t *testing.T) {
	t.Parallel()
	var mutations atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			mutations.Add(1)
			w.WriteHeader(503)
			if _, err := w.Write([]byte(`{"ok":false,"data":null,"error":{"code":"SQL password private","message":"private"}}`)); err != nil {
				t.Error(err)
			}
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": []map[string]any{{"id": "prf_demo", "org_id": "org_demo", "project_id": "prj_demo", "name": "Display", "database_type": "postgresql", "connector_id": "con_demo", "database_url": "private", "env": "private", "rows": []string{"private"}}}}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	client, err := cloud.NewClient(server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	profiles, err := client.Profiles(t.Context(), "org_demo", "prj_demo")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private") {
		t.Fatal("private response fields were projected")
	}
	_, err = client.CreateJob(t.Context(), "org_demo", "prj_demo", cloud.JobInput{Kind: "clone.create", Name: "Base", ProfileID: "prf_demo", IdempotencyKey: "stable-intent-0001"})
	var publicErr *cloud.Error
	if !errors.As(err, &publicErr) || publicErr.Code != "unavailable" || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe error mapping: %v", err)
	}
	if mutations.Load() != 1 {
		t.Fatalf("mutation requests = %d, want 1", mutations.Load())
	}
}

func TestMalformedJobNeverAppearsReady(t *testing.T) {
	t.Parallel()
	for _, fields := range []map[string]string{{"status": "ready"}, {"failure_code": "private SQL error"}, {"cleanup_state": "secure"}, {"org_id": "org_other"}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			job := map[string]any{"id": "job_demo", "org_id": "org_demo", "project_id": "prj_demo", "resource_id": "res_demo", "connector_id": "con_demo", "kind": "clone.create", "status": "succeeded", "failure_code": "", "cleanup_state": "not_required", "created_at": time.Now().UTC()}
			for k, v := range fields {
				job[k] = v
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": job}); err != nil {
				t.Error(err)
			}
		}))
		client, err := cloud.NewClient(server.URL, "")
		if err != nil {
			t.Fatal(err)
		}
		got, err := client.Job(t.Context(), "org_demo", "prj_demo", "job_demo")
		client.Close()
		server.Close()
		if err == nil || got.ID != "" {
			t.Errorf("malformed job %v = %v, err %v, want empty failure", fields, got, err)
		}
	}
}
