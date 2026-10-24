package account

import (
	"encoding/json"
	"errors"

	"github.com/forkden/cli/api/cloud"
	"github.com/zalando/go-keyring"
)

// WorkCredentials stores only origin-bound short-lived fork capabilities.
type WorkCredentials interface {
	Get(id string) (Credential, error)
	Set(id string, credential Credential) error
	Delete(id string) error
}

// WorkKeyring separates fork credentials from account credentials with no plaintext fallback.
type WorkKeyring struct{}

const workKeyringService = "com.forkden.cli.work"

// Get retrieves a work credential without exposing secret-store diagnostics.
func (WorkKeyring) Get(id string) (Credential, error) {
	record, err := keyring.Get(workKeyringService, id)
	if err != nil {
		return Credential{}, errors.New("work session unavailable; unlock the OS keyring or create a session")
	}
	var credential Credential
	if len(record) > 4096 || json.Unmarshal([]byte(record), &credential) != nil || !validWorkCredential(credential) {
		return Credential{}, errors.New("invalid work credential")
	}
	return credential, nil
}

// Set writes the origin-bound capability under its opaque session ID.
func (WorkKeyring) Set(id string, credential Credential) error {
	if !cloud.ValidWorkSessionID(id) || !validWorkCredential(credential) {
		return errors.New("invalid work credential")
	}
	record, err := json.Marshal(credential)
	if err != nil {
		return errors.New("cannot encode work credential")
	}
	if err := keyring.Set(workKeyringService, id, string(record)); err != nil {
		return errors.New("cannot save work credential; an unlocked OS keyring is required")
	}
	return nil
}

// Delete removes a revoked work credential idempotently.
func (WorkKeyring) Delete(id string) error {
	if err := keyring.Delete(workKeyringService, id); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return errors.New("cannot remove revoked work credential")
	}
	return nil
}

func validWorkCredential(c Credential) bool {
	origin, err := cloud.Origin(c.APIURL)
	return err == nil && origin == c.APIURL && cloud.ValidWorkToken(c.Token)
}
