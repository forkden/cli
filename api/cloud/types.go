// Package cloud defines the public account and copy-job API without private engine dependencies.
package cloud

import "time"

// Scope is the fixed capability granted to the OSS CLI.
const Scope = "metadata:read jobs:write"

// User contains only the public account identity.
type User struct {
	ID    string `json:"id"`
	Login string `json:"login"`
}

// Session contains the expiry and capabilities of a revocable CLI account token.
type Session struct {
	User      User      `json:"user"`
	ExpiresAt time.Time `json:"expires_at"`
	Scope     string    `json:"scope"`
}

// Device contains a one-time device credential; never serialize it to CLI output.
type Device struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// Grant contains the account bearer returned once after browser consent.
type Grant struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Session
}

// Organization is an account's membership in a workspace.
type Organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// Project groups metadata within one organization.
type Project struct {
	ID    string `json:"id"`
	OrgID string `json:"org_id"`
	Name  string `json:"name"`
}

// Profile is display metadata and an opaque worker link, never a connection reference.
type Profile struct {
	ID           string `json:"id"`
	OrgID        string `json:"org_id"`
	ProjectID    string `json:"project_id"`
	Name         string `json:"name"`
	DatabaseType string `json:"database_type"`
	ConnectorID  string `json:"connector_id"`
}

// Job contains only the public operation metadata.
type Job struct {
	ID                string     `json:"id"`
	OrgID             string     `json:"org_id"`
	ProjectID         string     `json:"project_id"`
	ConnectorID       string     `json:"connector_id"`
	ResourceID        string     `json:"resource_id"`
	Kind              string     `json:"kind"`
	Name              string     `json:"name"`
	ProfileID         string     `json:"profile_id"`
	CloneID           string     `json:"clone_id"`
	CloneVersion      int        `json:"clone_version"`
	TTLSeconds        int        `json:"ttl_seconds"`
	Status            string     `json:"status"`
	FailureCode       string     `json:"failure_code"`
	CleanupState      string     `json:"cleanup_state"`
	CreatedAt         time.Time  `json:"created_at"`
	StartedAt         *time.Time `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at"`
	ResourceExpiresAt *time.Time `json:"resource_expires_at"`
	CancelRequestedAt *time.Time `json:"cancel_requested_at"`
}

// JobInput requests a metadata-only copy intent; the worker owns all database I/O.
type JobInput struct {
	ResourceID     string `json:"resource_id,omitempty"`
	Kind           string `json:"kind"`
	Name           string `json:"name"`
	ProfileID      string `json:"profile_id"`
	CloneID        string `json:"clone_id"`
	CloneVersion   int    `json:"clone_version"`
	TTLSeconds     int    `json:"ttl_seconds"`
	IdempotencyKey string `json:"idempotency_key"`
}

// Resource is the worker-confirmed lifecycle of a clone or fork, independent of job history.
type Resource struct {
	ID            string     `json:"id"`
	OrgID         string     `json:"org_id"`
	ProjectID     string     `json:"project_id"`
	ConnectorID   string     `json:"connector_id"`
	Kind          string     `json:"kind"`
	Name          string     `json:"name"`
	ProfileID     string     `json:"profile_id"`
	CloneID       string     `json:"clone_id"`
	CloneVersion  int        `json:"clone_version"`
	LatestVersion int        `json:"latest_version"`
	Status        string     `json:"status"`
	LastJobID     string     `json:"last_job_id"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	ExpiresAt     *time.Time `json:"expires_at"`
}

// CloneVersion describes publication state; it never contains a connection or snapshot payload.
type CloneVersion struct {
	CloneID   string     `json:"clone_id"`
	Number    int        `json:"number"`
	JobID     string     `json:"job_id"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	ReadyAt   *time.Time `json:"ready_at"`
}

// Terminal reports whether polling should stop for this authoritative job state.
func (j Job) Terminal() bool {
	return j.Status == "succeeded" || j.Status == "failed" || j.Status == "cancelled" || j.Status == "needs_attention"
}
