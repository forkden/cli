package account_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/forkden/cli/internal/account"
)

func TestSettingsRoundTripPrivatePermissionsAndWriterLock(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "cli")
	store, err := account.Open(dir)
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
	if err := store.Lock(); err == nil {
		t.Fatal("second writer acquired the same config lock")
	}
	cfg := account.Config{APIURL: "https://example.com", CredentialID: "cred_demo", UserID: "usr_demo", OrgID: "org_demo", ProjectID: "prj_demo"}
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.Unlock(); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil || loaded != cfg {
		t.Fatalf("Load = %v, err %v, want %v", loaded, err, cfg)
	}
	//nolint:gosec // This reads only the test's own temporary configuration file.
	data, err := os.ReadFile(filepath.Join(dir, "cloud.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "access_token") {
		t.Fatal("credential field stored in settings")
	}
	info, err := os.Stat(filepath.Join(dir, "cloud.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("settings permissions = %o, want 0600", info.Mode().Perm())
	}
}

func TestSettingsRejectSecretsAndSymlink(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "cli")
	store, err := account.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	path := filepath.Join(dir, "cloud.json")
	if err := os.WriteFile(path, []byte(`{"api_url":"https://example.com","access_token":"private"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("plaintext token accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), path); err != nil {
		t.Skip("symlink unavailable")
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("symlink config accepted")
	}
}
