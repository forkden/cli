package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

type memoryCredentials struct {
	values  map[string]account.Credential
	failSet bool
}

func (m *memoryCredentials) Get(id string) (account.Credential, error) {
	v, ok := m.values[id]
	if !ok {
		return account.Credential{}, errors.New("missing test credential")
	}
	return v, nil
}
func (m *memoryCredentials) Set(id string, credential account.Credential) error {
	if m.failSet {
		return errors.New("test keyring locked")
	}
	m.values[id] = credential
	return nil
}
func (m *memoryCredentials) Delete(id string) error { delete(m.values, id); return nil }

type cliFixture struct {
	server      *httptest.Server
	app         cloudApp
	dir, token  string
	mu          sync.Mutex
	devicePolls int
	intervals   []time.Duration
	intents     []cloud.JobInput
	jobReads    int
	revoked     bool
}

func newCLIFixture(t *testing.T) *cliFixture {
	t.Helper()
	f := &cliFixture{dir: filepath.Join(t.TempDir(), "config"), token: "fda_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))}
	f.app = cloudApp{credentials: &memoryCredentials{values: map[string]account.Credential{}}, openBrowser: func(context.Context, string) error { return errors.New("browser unavailable") }, pause: func(ctx context.Context, d time.Duration) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		f.intervals = append(f.intervals, d)
		return nil
	}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		success := func(data any) {
			if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data}); err != nil {
				t.Error(err)
			}
		}
		session := cloud.Session{User: cloud.User{ID: "usr_demo", Login: "example"}, ExpiresAt: time.Now().Add(7 * 24 * time.Hour), Scope: cloud.Scope}
		if r.URL.Path == "/auth/cli/device" {
			success(cloud.Device{DeviceCode: "fdc_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32)), UserCode: "ABCDE-23456", VerificationURI: f.server.URL + "/device", ExpiresIn: 600, Interval: 5})
			return
		}
		if r.URL.Path == "/auth/cli/token" {
			f.devicePolls++
			if f.devicePolls == 1 {
				w.WriteHeader(400)
				if _, err := w.Write([]byte(`{"ok":false,"data":null,"error":{"code":"slow_down"}}`)); err != nil {
					t.Error(err)
				}
				return
			}
			success(cloud.Grant{AccessToken: f.token, TokenType: "Bearer", Session: session})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+f.token || r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" {
			t.Error("CLI credentials were missing or mixed with browser authority")
			w.WriteHeader(401)
			return
		}
		job := cloud.Job{ID: "job_demo", OrgID: "org_demo", ProjectID: "prj_demo", ConnectorID: "con_demo", ResourceID: "res_demo", Name: "Baseline", Kind: "clone.create", ProfileID: "prf_demo", Status: "queued", CleanupState: "not_required", CreatedAt: time.Now().UTC()}
		switch {
		case r.URL.Path == "/v1/cli/session":
			success(session)
		case r.URL.Path == "/auth/cli/logout":
			f.revoked = true
			success(map[string]bool{"signed_out": true})
		case r.URL.Path == "/v1/orgs":
			success([]cloud.Organization{{ID: "org_demo", Name: "Team", Role: "owner"}})
		case r.URL.Path == "/v1/orgs/org_demo/projects":
			success([]cloud.Project{{ID: "prj_demo", OrgID: "org_demo", Name: "Commerce"}})
		case strings.HasSuffix(r.URL.Path, "/profiles"):
			success([]cloud.Profile{{ID: "prf_demo", OrgID: "org_demo", ProjectID: "prj_demo", Name: "Display", DatabaseType: "postgresql", ConnectorID: "con_demo"}})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/jobs"):
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			if strings.Contains(string(data), "env") || strings.Contains(string(data), "sql") {
				t.Error("private operation fields reached account API")
			}
			var input cloud.JobInput
			if err := json.Unmarshal(data, &input); err != nil {
				t.Error(err)
				return
			}
			f.intents = append(f.intents, input)
			job.Kind, job.Name, job.CloneID, job.CloneVersion, job.TTLSeconds = input.Kind, input.Name, input.CloneID, input.CloneVersion, input.TTLSeconds
			success(job)
		case strings.HasSuffix(r.URL.Path, "/jobs/job_demo/cancel"):
			job.Status = "cancelled"
			job.FailureCode = "cancelled"
			success(job)
		case strings.HasSuffix(r.URL.Path, "/jobs/job_demo"):
			f.jobReads++
			if f.jobReads == 1 {
				job.Status = "running"
				job.CleanupState = "unknown"
			} else {
				job.Status = "succeeded"
			}
			success(job)
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			success([]cloud.Job{job})
		default:
			t.Errorf("unexpected cloud path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *cliFixture) run(t *testing.T, args ...string) (map[string]json.RawMessage, string, error) {
	t.Helper()
	var out, progress bytes.Buffer
	args = append([]string{"--json", "--config_dir", f.dir}, args...)
	err := runWithCloud(t.Context(), args, &out, &progress, f.app)
	var envelope map[string]json.RawMessage
	if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
		t.Fatalf("CLI output is not one JSON envelope: %s, %v", out.String(), decodeErr)
	}
	if strings.Contains(out.String(), f.token) || strings.Contains(progress.String(), f.token) || strings.Contains(progress.String(), "fdc_") {
		t.Fatal("credential printed by CLI")
	}
	return envelope, progress.String(), err
}

func TestCloudCLIAccountSelectionCopyJobsAndLogout(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	_, progress, err := f.run(t, "--api_url", f.server.URL, "login", "--no-browser")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(progress, "ABCDE-23456") || len(f.intervals) != 2 || f.intervals[0] != 5*time.Second || f.intervals[1] != 10*time.Second {
		t.Fatalf("login poll intervals = %v, want [5s 10s]", f.intervals)
	}
	data, err := os.ReadFile(filepath.Join(f.dir, "cloud.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), f.token) || strings.Contains(string(data), "access_token") {
		t.Fatal("account token written to config")
	}
	for _, args := range [][]string{{"whoami"}, {"cloud", "org", "list"}, {"cloud", "project", "list", "--org", "org_demo"}, {"cloud", "project", "use", "prj_demo", "--org", "org_demo"}, {"cloud", "db", "list"}, {"cloud", "clone", "create", "Baseline", "--profile", "prf_demo", "--request_id", "same-intent-00001", "--wait"}, {"cloud", "fork", "create", "Experiment", "--clone", "res_demo", "--ttl", "15m", "--request_id", "same-fork-0000001"}, {"cloud", "job", "cancel", "job_demo"}, {"cloud", "clone", "list"}, {"cloud", "clone", "show", "res_demo"}, {"logout"}} {
		if _, _, err := f.run(t, args...); err != nil {
			t.Fatalf("run %v = %v, want success", args, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.intents) != 2 || f.intents[0].IdempotencyKey != "same-intent-00001" || f.intents[1].CloneID != "res_demo" || f.intents[1].CloneVersion != 1 || f.intents[1].TTLSeconds != 900 || !f.revoked {
		t.Fatalf("intent lineage / logout incorrect: %v", f.intents)
	}
	if len(f.app.credentials.(*memoryCredentials).values) != 0 {
		t.Error("logout retained account credential")
	}
}

func TestCloudCLIOriginPinningUnsupportedPrivateFlagsAndGrantCleanup(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	if _, _, err := f.run(t, "--api_url", f.server.URL, "login", "--no-browser"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--api_url", "https://other.example", "whoami"}, {"--socket", "engine.sock", "cloud", "org", "list"}, {"cloud", "query", "--sql", "select 'private'"}} {
		if _, _, err := f.run(t, args...); err == nil {
			t.Errorf("run %v accepted unsafe mode or origin", args)
		}
	}
	if _, _, err := f.run(t, "cloud", "project", "use", "prj_demo", "--org", "org_demo"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.run(t, "cloud", "clone", "create", "Base", "--profile", "prf_demo", "--target_env", "PRIVATE_DB_URL"); err == nil {
		t.Error("cloud mode accepted a DB environment reference")
	}
	failed := newCLIFixture(t)
	failed.app.credentials.(*memoryCredentials).failSet = true
	if _, _, err := failed.run(t, "--api_url", failed.server.URL, "login", "--no-browser"); err == nil {
		t.Fatal("locked keyring login succeeded")
	}
	failed.mu.Lock()
	defer failed.mu.Unlock()
	if !failed.revoked {
		t.Fatal("failed login persistence did not revoke the unsaved grant")
	}
}

func TestEditedSettingsCannotRedirectKeyringCredential(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	if _, _, err := f.run(t, "--api_url", f.server.URL, "login", "--no-browser"); err != nil {
		t.Fatal(err)
	}
	store, err := account.Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := store.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Unlock(); err != nil {
			t.Error(err)
		}
	}()
	cfg, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.APIURL = "https://other.example"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	_, _, err = f.run(t, "whoami")
	if err == nil {
		t.Fatal("edited settings redirected an origin-bound keyring credential")
	}
}
