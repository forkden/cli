//go:build integration

package main

import (
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/forkden/cli/internal/account"
)

// TestCloudCLIProcess runs the public command path with a disposable credential
// adapter supplied through stdin. Production still requires the OS keyring.
func TestCloudCLIProcess(t *testing.T) {
	if os.Getenv("FORKDEN_CLI_E2E_HELPER") != "yes" {
		t.Skip("cross-repository helper only")
	}
	var input struct {
		Directory               string
		Config                  account.Config
		AccountToken, WorkToken string
		Args                    []string
	}
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 32768))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		t.Fatal("invalid private fixture input")
	}
	store, err := account.Open(input.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(input.Config); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	app := cloudApp{
		credentials:     &memoryCredentials{values: map[string]account.Credential{input.Config.CredentialID: {APIURL: input.Config.APIURL, Token: input.AccountToken}}},
		workCredentials: &memoryCredentials{values: map[string]account.Credential{input.Config.WorkCredentialID: {APIURL: input.Config.APIURL, Token: input.WorkToken}}},
	}
	args := append([]string{"--json", "--config_dir", input.Directory}, input.Args...)
	if err := runWithCloud(t.Context(), args, os.Stdout, os.Stderr, app); err != nil {
		t.Fatal(err)
	}
}
