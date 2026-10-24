package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// WorkSession is metadata for a short-lived capability bound to one fork.
type WorkSession struct {
	ID          string     `json:"id"`
	OrgID       string     `json:"org_id"`
	ProjectID   string     `json:"project_id"`
	ResourceID  string     `json:"resource_id"`
	ConnectorID string     `json:"connector_id"`
	UserID      string     `json:"user_id"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at"`
	AllowRows   bool       `json:"allow_rows"`
}

// WorkSessionGrant contains a one-time secret; CLI output must project Session only.
type WorkSessionGrant struct {
	Session WorkSession `json:"session"`
	Token   string      `json:"session_token"`
}

// WorkInput carries SQL only on the separate authenticated execution channel.
type WorkInput struct {
	ID   string `json:"request_id"`
	Kind string `json:"kind"`
	SQL  string `json:"sql,omitempty"`
}

// WorkResult contains transient authorized output and its explicit execution status.
type WorkResult struct {
	Kind      string          `json:"kind"`
	Status    string          `json:"status"`
	ErrorCode string          `json:"error_code"`
	Operation json.RawMessage `json:"operation,omitempty"`
	Schema    json.RawMessage `json:"schema,omitempty"`
	Report    json.RawMessage `json:"report,omitempty"`
	SQL       string          `json:"sql,omitempty"`
	History   json.RawMessage `json:"history,omitempty"`
}

// CreateWorkSession requests explicitly scoped SQL access using the account token.
func (c *Client) CreateWorkSession(ctx context.Context, org, project, resource string, ttl int, rows bool) (WorkSessionGrant, error) {
	if !ValidID(org) || !ValidID(project) || !ValidID(resource) {
		return WorkSessionGrant{}, failure("invalid_input")
	}
	var out WorkSessionGrant
	err := c.request(ctx, http.MethodPost, "/v1/orgs/"+org+"/projects/"+project+"/resources/"+resource+"/sessions", struct {
		TTL  int  `json:"ttl_seconds"`
		Rows bool `json:"allow_rows"`
	}{TTL: ttl, Rows: rows}, &out)
	if err == nil && (!ValidWorkToken(out.Token) || !ValidWorkSessionID(out.Session.ID) || out.Session.OrgID != org || out.Session.ProjectID != project || out.Session.ResourceID != resource || !time.Now().Before(out.Session.ExpiresAt)) {
		return WorkSessionGrant{}, failure("invalid_response")
	}
	return out, err
}

// RevokeWorkSession invalidates a session using the account authority.
func (c *Client) RevokeWorkSession(ctx context.Context, org, project, session string) error {
	if !ValidID(org) || !ValidID(project) || !ValidWorkSessionID(session) {
		return failure("invalid_input")
	}
	return c.request(ctx, http.MethodDelete, "/v1/orgs/"+org+"/projects/"+project+"/sessions/"+session, struct{}{}, nil)
}

// WorkClient pins a separate fork credential to its canonical origin.
type WorkClient struct {
	origin, token string
	http          *http.Client
}

// NewWorkClient permits only HTTPS or explicit loopback development origins.
func NewWorkClient(origin, token string) (*WorkClient, error) {
	canonical, err := Origin(origin)
	if err != nil || !ValidWorkToken(token) {
		return nil, failure("invalid_input")
	}
	return &WorkClient{origin: canonical, token: token, http: &http.Client{Timeout: 70 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Close releases idle connections without changing the session.
func (c *WorkClient) Close() { c.http.CloseIdleConnections() }

// Session checks current membership/revocation rather than trusting local selection.
func (c *WorkClient) Session(ctx context.Context) (WorkSession, error) {
	var out WorkSession
	err := c.request(ctx, http.MethodGet, "/v1/work/session", nil, &out)
	return out, err
}

// Execute never retries SQL automatically, including after a lost response.
func (c *WorkClient) Execute(ctx context.Context, input WorkInput) (WorkResult, error) {
	if !regexp.MustCompile(`^wrq_[a-f0-9]{32}$`).MatchString(input.ID) || len(input.SQL) > 64<<10 {
		return WorkResult{}, failure("invalid_input")
	}
	var out WorkResult
	err := c.request(ctx, http.MethodPost, "/v1/work/execute", input, &out)
	if err != nil {
		return out, err
	}
	if out.Status != "succeeded" {
		code := out.ErrorCode
		if code == "" {
			code = "unknown_outcome"
		}
		return out, &Error{Code: code, Message: "Work did not complete successfully; inspect the execution history before retrying."}
	}
	return out, nil
}

func (c *WorkClient) request(ctx context.Context, method, path string, input, out any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil || len(data) > 512<<10 {
			return failure("invalid_input")
		}
		body = bytes.NewReader(data)
	}
	r, err := http.NewRequestWithContext(ctx, method, c.origin+path, body)
	if err != nil {
		return failure("invalid_input")
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("Accept", "application/json")
	if input != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(r)
	if err != nil {
		return &Error{Code: "unknown_outcome", Message: "Execution response unavailable; inspect history before submitting SQL again."}
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096)); _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return failure("invalid_response")
	}
	var envelope struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return failure("invalid_response")
	}
	if response.StatusCode != http.StatusOK || !envelope.OK {
		if envelope.Error != nil {
			return failure(envelope.Error.Code)
		}
		return failure("invalid_response")
	}
	if out == nil {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(envelope.Data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return failure("invalid_response")
	}
	return nil
}

// ValidWorkToken keeps work credentials separate from account/worker credentials.
func ValidWorkToken(token string) bool {
	return regexp.MustCompile(`^fws_[A-Za-z0-9_-]{43}$`).MatchString(token)
}

// ValidWorkSessionID validates only opaque work-session references.
func ValidWorkSessionID(id string) bool {
	return regexp.MustCompile(`^wss_[a-f0-9]{32}$`).MatchString(id)
}
