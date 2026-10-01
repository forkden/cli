package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"time"
)

// Client calls a local engine without importing or embedding its implementation.
type Client struct {
	http *http.Client
}

// NewClient creates a client for an absolute Unix socket path. No network proxy is used.
func NewClient(socket string) (*Client, error) {
	if socket == "" || !filepath.IsAbs(socket) {
		return nil, errors.New("engine socket path must be absolute")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 2 * time.Minute,
	}
	return &Client{http: &http.Client{
		Transport: transport,
		Timeout:   2 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}, nil
}

// Execute performs one command without application retries; failures may have reached the engine.
func (c *Client) Execute(ctx context.Context, action Action, input any) (Response, error) {
	params, err := json.Marshal(input)
	if err != nil {
		return Response{}, errors.New("command input cannot be encoded")
	}
	body, err := json.Marshal(Request{APIVersion: Version, Action: action, Params: params})
	if err != nil || len(body) > MaxRequestBytes {
		return Response{}, errors.New("command exceeds the protocol request limit")
	}
	return c.request(ctx, http.MethodPost, "/v1/commands", body)
}

// Status reads engine version and readiness without accessing a database.
func (c *Client) Status(ctx context.Context) (Response, error) {
	return c.request(ctx, http.MethodGet, "/v1/status", nil)
}

func (c *Client) request(ctx context.Context, method, path string, body []byte) (response Response, retErr error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://forkden"+path, bytes.NewReader(body))
	if err != nil {
		return Response{}, errors.New("engine request cannot be created")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	reply, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, errors.New("engine connection failed; start forkden-engine serve and check the socket; inspect history before retrying a mutation")
	}
	defer func() {
		if err := reply.Body.Close(); err != nil {
			retErr = errors.Join(retErr, errors.New("engine response could not be closed"))
		}
	}()
	data, err := io.ReadAll(io.LimitReader(reply.Body, MaxResponseBytes+1))
	if ctx.Err() != nil {
		return Response{}, ctx.Err()
	}
	if err != nil || len(data) > MaxResponseBytes {
		return Response{}, errors.New("engine response is incomplete or exceeds the protocol limit; inspect history before retrying")
	}
	if err := json.Unmarshal(data, &response); err != nil || response.APIVersion != Version || (response.OK == (response.Error != nil)) {
		return Response{}, errors.New("engine returned an incompatible protocol response")
	}
	if response.OK && (reply.StatusCode < 200 || reply.StatusCode >= 300) {
		return Response{}, errors.New("engine returned an inconsistent response status")
	}
	if response.Error != nil {
		return response, response.Error
	}
	return response, nil
}
