package account

import (
	"encoding/json"
	"errors"

	"github.com/forkden/cli/api/cloud"
	"github.com/zalando/go-keyring"
)

// Credential binds an account token to its canonical API origin inside the secret store.
type Credential struct {
	APIURL string `json:"api_url"`
	Token  string `json:"token"`
}

// Credentials is the account-token storage port; the config stores only its opaque key.
type Credentials interface {
	Get(id string) (Credential, error)
	Set(id string, credential Credential) error
	Delete(id string) error
}

// Keyring uses the OS secret store, without a plaintext fallback.
type Keyring struct{}

var _ Credentials = Keyring{}

const keyringService = "com.forkden.cli.account"

// Get retrieves a credential without forwarding keyring diagnostics to output.
func (Keyring) Get(id string) (Credential, error) {
	record, err := keyring.Get(keyringService, id)
	if err != nil {
		return Credential{}, errors.New("account credential is unavailable; unlock your OS keyring or run login again")
	}
	var credential Credential
	if len(record) > 4096 || json.Unmarshal([]byte(record), &credential) != nil || !validCredential(credential) {
		return Credential{}, errors.New("account credential is invalid; run login again")
	}
	return credential, nil
}

// Set persists an origin-bound account credential under an opaque key.
func (Keyring) Set(id string, credential Credential) error {
	if !validCredential(credential) {
		return errors.New("invalid origin-bound account credential")
	}
	record, err := json.Marshal(credential)
	if err != nil {
		return errors.New("cannot encode account credential")
	}
	if err := keyring.Set(keyringService, id, string(record)); err != nil {
		return errors.New("cannot save account credential; an unlocked OS keyring is required")
	}
	return nil
}

func validCredential(credential Credential) bool {
	origin, err := cloud.Origin(credential.APIURL)
	return err == nil && origin == credential.APIURL && cloud.ValidToken(credential.Token)
}

// Delete removes an already revoked account credential idempotently.
func (Keyring) Delete(id string) error {
	if err := keyring.Delete(keyringService, id); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return errors.New("cannot remove the revoked account credential from the OS keyring")
	}
	return nil
}
