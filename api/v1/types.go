// Package v1 defines the public Forkden engine protocol and Unix socket client.
package v1

import "encoding/json"

const (
	// Version identifies the compatible wire protocol, independent of binary versions.
	Version = "1"
	// MaxRequestBytes bounds a complete command request, including SQL.
	MaxRequestBytes = 256 << 10
	// MaxResponseBytes bounds a complete response, including review data.
	MaxResponseBytes = 16 << 20
)

// Action names an engine operation; the engine owns all database behavior.
type Action string

// Supported protocol operations.
const (
	ProfileAdd    Action = "profile.add"
	ProfileList   Action = "profile.list"
	ProfileShow   Action = "profile.show"
	ProfileCheck  Action = "profile.check"
	ProfileUpdate Action = "profile.update"
	ProfileRemove Action = "profile.remove"
	ForkCreate    Action = "fork.create"
	ForkList      Action = "fork.list"
	ForkClose     Action = "fork.close"
	ForkPrune     Action = "fork.prune"
	Query         Action = "query"
	Check         Action = "check"
	History       Action = "history"
	Review        Action = "review"
	Export        Action = "export"
)

// Request carries one typed operation input. It never contains a database URL.
type Request struct {
	APIVersion string          `json:"api_version"`
	Action     Action          `json:"action"`
	Params     json.RawMessage `json:"params"`
}

// Response carries only public engine data and a stable error code.
type Response struct {
	APIVersion string          `json:"api_version"`
	OK         bool            `json:"ok"`
	Data       json.RawMessage `json:"data"`
	Error      *Error          `json:"error,omitempty"`
}

// Error is a credential-free engine failure. Code determines the CLI exit code.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error returns the public failure description.
func (e *Error) Error() string { return e.Message }

// ProfileInput names a database profile and an environment reference on the engine.
type ProfileInput struct {
	Name   string `json:"name"`
	URLEnv string `json:"url_env"`
}

// ProfileName identifies one registered source profile.
type ProfileName struct {
	Name string `json:"name"`
}

// CreateInput requests an isolated fork. TTLSeconds must be between 1 and 86400.
type CreateInput struct {
	Source     string `json:"source"`
	TargetEnv  string `json:"target_env"`
	TTLSeconds int64  `json:"ttl_seconds"`
}

// ForkID identifies one engine-owned fork.
type ForkID struct {
	ID string `json:"id"`
}

// QueryInput contains one SQL statement against a fork, never a source connection.
type QueryInput struct {
	ForkID string `json:"fork_id"`
	SQL    string `json:"sql"`
}

// ExportData contains artifacts to be written on the CLI machine.
type ExportData struct {
	SQL    string          `json:"sql"`
	Report json.RawMessage `json:"report"`
}
