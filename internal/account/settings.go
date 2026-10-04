// Package account stores a pinned API selection and opaque OS keyring references.
package account

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"

	"github.com/forkden/cli/api/cloud"
)

// Config contains no account credential, database URL or secret reference.
type Config struct {
	APIURL       string `json:"api_url"`
	CredentialID string `json:"credential_id"`
	UserID       string `json:"user_id"`
	OrgID        string `json:"org_id"`
	ProjectID    string `json:"project_id"`
}

// Store confines settings and atomic writes to a private configuration directory.
type Store struct{ root *os.Root }

// Open creates or opens a private directory, rejecting symlinks and broad Unix permissions.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.New("cannot create CLI configuration directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return nil, errors.New("CLI configuration directory must be private (0700) and not a symlink")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.New("cannot open CLI configuration directory")
	}
	return &Store{root: root}, nil
}

// Close releases the configuration directory handle.
func (s *Store) Close() error { return s.root.Close() }

// Lock serializes login, logout and selection changes across CLI processes.
// After a killed writer, the user can remove .write-lock once no writer remains.
func (s *Store) Lock() error {
	if err := s.root.Mkdir(".write-lock", 0o700); err != nil {
		return errors.New("CLI settings are busy; if a writer crashed, remove .write-lock in the config directory after it exits")
	}
	return nil
}

// Unlock releases an acquired settings writer lock.
func (s *Store) Unlock() error { return s.root.Remove(".write-lock") }

// Load validates a metadata-only configuration before using any keyring credential.
func (s *Store) Load() (Config, error) {
	info, err := s.root.Lstat("cloud.json")
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return Config{}, errors.New("CLI cloud.json must be a private regular file (0600)")
	}
	file, err := s.root.Open("cloud.json")
	if err != nil {
		return Config{}, errors.New("cannot read CLI settings")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 4097))
	decoder.DisallowUnknownFields()
	var cfg Config
	decodeErr := decoder.Decode(&cfg)
	endErr := decoder.Decode(&struct{}{})
	if err := errors.Join(decodeErr, file.Close()); err != nil || !errors.Is(endErr, io.EOF) {
		return Config{}, errors.New("invalid CLI settings; credential values do not belong in cloud.json")
	}
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Save atomically replaces metadata settings. The caller must hold Lock.
func (s *Store) Save(cfg Config) (retErr error) {
	if err := validate(cfg); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return errors.New("cannot encode CLI settings")
	}
	file, err := s.root.OpenFile(".cloud.json.tmp", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("cannot create atomic CLI settings file; check for an interrupted writer")
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, s.root.Remove(".cloud.json.tmp"))
		}
	}()
	_, writeErr := file.Write(append(data, '\n'))
	syncErr := file.Sync()
	if err := errors.Join(writeErr, syncErr, file.Close()); err != nil {
		return errors.New("cannot persist CLI settings")
	}
	if err := s.root.Rename(".cloud.json.tmp", "cloud.json"); err != nil {
		return errors.New("cannot replace CLI settings")
	}
	return nil
}

func validate(cfg Config) error {
	if cfg.APIURL == "" {
		if cfg != (Config{}) {
			return errors.New("CLI settings are missing their API origin")
		}
		return nil
	}
	origin, err := cloud.Origin(cfg.APIURL)
	if err != nil || origin != cfg.APIURL || (cfg.CredentialID != "" && !cloud.ValidID(cfg.CredentialID)) || (cfg.UserID != "" && !cloud.ValidID(cfg.UserID)) || (cfg.OrgID != "" && !cloud.ValidID(cfg.OrgID)) || (cfg.ProjectID != "" && !cloud.ValidID(cfg.ProjectID)) || (cfg.ProjectID != "" && cfg.OrgID == "") || ((cfg.CredentialID == "") != (cfg.UserID == "")) {
		return errors.New("invalid CLI account or workspace selection")
	}
	return nil
}
