package cloud

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Error is a stable public API/client failure. Transport and response details are discarded.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error returns a fixed, credential-free explanation.
func (e *Error) Error() string { return e.Message }

func failure(code string) *Error {
	message := "Forkden returned an invalid or unavailable response."
	switch code {
	case "invalid_input":
		message = "Check the command input."
	case "unauthorized":
		message = "Your CLI session has ended; run forkden login."
	case "forbidden":
		message = "Your account does not have permission for this operation."
	case "not_found":
		message = "This workspace or resource is no longer available."
	case "conflict":
		message = "The request conflicts with existing metadata or a workspace limit."
	case "rate_limited":
		message = "Too many requests; wait a minute before trying again."
	case "authorization_pending":
		message = "Waiting for browser approval."
	case "slow_down":
		message = "Poll less frequently."
	case "access_denied":
		message = "CLI access was declined in the browser."
	case "expired_token":
		message = "This login code expired or was already used; run login again."
	case "transport_error":
		message = "Cannot reach Forkden. A change may have completed; check job list or retry the same request ID."
	default:
		code = "unavailable"
	}
	return &Error{Code: code, Message: message}
}

// Client owns an origin-pinned, redirect-free account API connection.
type Client struct {
	origin, token string
	http          *http.Client
}

// Origin validates a canonical HTTPS origin, permitting HTTP only for loopback development.
func Origin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return "", failure("invalid_input")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
		return "", failure("invalid_input")
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

// ValidToken rejects cookies, worker tokens, whitespace and malformed account credentials.
func ValidToken(token string) bool {
	if len(token) != 47 || !strings.HasPrefix(token, "fda_") {
		return false
	}
	data, err := base64.RawURLEncoding.DecodeString(token[4:])
	return err == nil && len(data) == 32
}

// NewClient accepts only a canonical origin and a distinct account credential, or empty for login.
func NewClient(rawURL, token string) (*Client, error) {
	origin, err := Origin(rawURL)
	if err != nil {
		return nil, err
	}
	if token != "" && !ValidToken(token) {
		return nil, failure("unauthorized")
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, MaxConnsPerHost: 4, MaxIdleConnsPerHost: 2, ResponseHeaderTimeout: 10 * time.Second, TLSHandshakeTimeout: 5 * time.Second, IdleConnTimeout: 30 * time.Second}
	return &Client{origin: origin, token: token, http: &http.Client{Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Close releases idle HTTP connections owned by this client.
func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) request(ctx context.Context, method, path string, input, out any) (retErr error) {
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil || len(body) > 16<<10 {
			return failure("invalid_input")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.origin+path, bytes.NewReader(body))
	if err != nil {
		return failure("invalid_input")
	}
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return failure("transport_error")
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			retErr = errors.Join(retErr, failure("transport_error"))
		}
	}()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || len(data) > 1<<20 {
		return failure("transport_error")
	}
	var envelope struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &envelope) != nil || (envelope.OK == (envelope.Error != nil)) {
		return failure("unavailable")
	}
	if !envelope.OK {
		if response.StatusCode < 400 || response.StatusCode > 599 {
			return failure("unavailable")
		}
		return failure(envelope.Error.Code)
	}
	if response.StatusCode != http.StatusOK || len(envelope.Data) == 0 || bytes.Equal(envelope.Data, []byte("null")) || json.Unmarshal(envelope.Data, out) != nil {
		return failure("unavailable")
	}
	return nil
}

// StartDevice requests a user-initiated login; the browser URI must stay on the same origin.
func (c *Client) StartDevice(ctx context.Context) (Device, error) {
	var out Device
	err := c.request(ctx, "POST", "/auth/cli/device", struct{}{}, &out)
	if err != nil {
		return Device{}, err
	}
	if out.VerificationURI != c.origin+"/device" || len(out.DeviceCode) != 47 || !strings.HasPrefix(out.DeviceCode, "fdc_") || !regexp.MustCompile(`^[A-HJ-NP-Z2-9]{5}-[A-HJ-NP-Z2-9]{5}$`).MatchString(out.UserCode) || out.ExpiresIn < 1 || out.ExpiresIn > 600 || out.Interval < 5 || out.Interval > 60 {
		return Device{}, failure("unavailable")
	}
	return out, nil
}

// RedeemDevice polls for approval without displaying the device credential.
func (c *Client) RedeemDevice(ctx context.Context, code string) (Grant, error) {
	var out Grant
	err := c.request(ctx, "POST", "/auth/cli/token", struct {
		DeviceCode string `json:"device_code"`
	}{code}, &out)
	if err != nil {
		return Grant{}, err
	}
	if !ValidToken(out.AccessToken) || out.TokenType != "Bearer" || out.Scope != Scope || out.User.ID == "" || out.User.Login == "" || out.ExpiresAt.IsZero() {
		return Grant{}, failure("unavailable")
	}
	return out, nil
}

// Session verifies the credential before reporting account information.
func (c *Client) Session(ctx context.Context) (Session, error) {
	var out Session
	err := c.request(ctx, "GET", "/v1/cli/session", nil, &out)
	if err == nil && (out.Scope != Scope || out.User.ID == "" || out.User.Login == "" || out.ExpiresAt.IsZero()) {
		err = failure("unavailable")
	}
	return out, err
}

// Logout revokes the presented account token server-side.
func (c *Client) Logout(ctx context.Context) error {
	var out struct {
		SignedOut bool `json:"signed_out"`
	}
	err := c.request(ctx, "POST", "/auth/cli/logout", struct{}{}, &out)
	if err == nil && !out.SignedOut {
		return failure("unavailable")
	}
	return err
}

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,79}$`)

// ValidID rejects names or paths where a platform resource ID is required.
func ValidID(id string) bool { return idPattern.MatchString(id) }

func projectPath(orgID, projectID string) (string, error) {
	if !ValidID(orgID) || !ValidID(projectID) {
		return "", failure("invalid_input")
	}
	return "/v1/orgs/" + orgID + "/projects/" + projectID, nil
}

// Organizations lists current memberships; selection does not confer authorization.
func (c *Client) Organizations(ctx context.Context) ([]Organization, error) {
	var out []Organization
	err := c.request(ctx, "GET", "/v1/orgs", nil, &out)
	return out, err
}

// Projects lists accessible projects in an organization.
func (c *Client) Projects(ctx context.Context, orgID string) ([]Project, error) {
	if !ValidID(orgID) {
		return nil, failure("invalid_input")
	}
	var out []Project
	err := c.request(ctx, "GET", "/v1/orgs/"+orgID+"/projects", nil, &out)
	return out, err
}

// Profiles lists display metadata for the selected project.
func (c *Client) Profiles(ctx context.Context, orgID, projectID string) ([]Profile, error) {
	path, err := projectPath(orgID, projectID)
	if err != nil {
		return nil, err
	}
	var out []Profile
	err = c.request(ctx, "GET", path+"/profiles", nil, &out)
	return out, err
}

// Jobs lists bounded, authoritative copy job metadata.
func (c *Client) Jobs(ctx context.Context, orgID, projectID string) ([]Job, error) {
	path, err := projectPath(orgID, projectID)
	if err != nil {
		return nil, err
	}
	var out []Job
	err = c.request(ctx, "GET", path+"/jobs", nil, &out)
	if err != nil {
		return nil, err
	}
	for _, job := range out {
		if !validJob(job, orgID, projectID) {
			return nil, failure("unavailable")
		}
	}
	return out, err
}

// Job reads one scoped job for polling.
func (c *Client) Job(ctx context.Context, orgID, projectID, id string) (Job, error) {
	path, err := projectPath(orgID, projectID)
	if err != nil {
		return Job{}, err
	}
	if !ValidID(id) {
		return Job{}, failure("invalid_input")
	}
	var out Job
	err = c.request(ctx, "GET", path+"/jobs/"+id, nil, &out)
	if err != nil {
		return Job{}, err
	}
	if !validJob(out, orgID, projectID) || out.ID != id {
		return Job{}, failure("unavailable")
	}
	return out, err
}

// CreateJob submits one idempotent intent. The client never retries mutations automatically.
func (c *Client) CreateJob(ctx context.Context, orgID, projectID string, input JobInput) (Job, error) {
	path, err := projectPath(orgID, projectID)
	if err != nil {
		return Job{}, err
	}
	var out Job
	err = c.request(ctx, "POST", path+"/jobs", input, &out)
	if err != nil {
		return Job{}, err
	}
	if !validJob(out, orgID, projectID) {
		return Job{}, failure("unavailable")
	}
	return out, err
}

// CancelJob cancels queued work only; it does not delete a database.
func (c *Client) CancelJob(ctx context.Context, orgID, projectID, id string) (Job, error) {
	path, err := projectPath(orgID, projectID)
	if err != nil {
		return Job{}, err
	}
	if !ValidID(id) {
		return Job{}, failure("invalid_input")
	}
	var out Job
	err = c.request(ctx, "POST", path+"/jobs/"+id+"/cancel", struct{}{}, &out)
	if err != nil {
		return Job{}, err
	}
	if !validJob(out, orgID, projectID) || out.ID != id {
		return Job{}, failure("unavailable")
	}
	return out, err
}

func validJob(j Job, orgID, projectID string) bool {
	if !ValidID(j.ID) || !ValidID(j.ResourceID) || !ValidID(j.ConnectorID) || j.OrgID != orgID || j.ProjectID != projectID || j.CreatedAt.IsZero() {
		return false
	}
	if j.Kind != "clone.create" && j.Kind != "fork.create" {
		return false
	}
	if j.Status != "queued" && j.Status != "running" && !j.Terminal() {
		return false
	}
	switch j.FailureCode {
	case "", "binding_missing", "execution_failed", "lease_lost", "interrupted", "authorization_lost", "queue_expired", "cancelled":
	default:
		return false
	}
	switch j.CleanupState {
	case "not_required", "complete", "pending", "unknown":
	default:
		return false
	}
	return true
}
