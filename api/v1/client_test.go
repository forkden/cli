package v1

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func testClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	// Short paths also fit macOS's Unix socket path limit.
	dir, err := os.MkdirTemp("", "fd-api-")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "engine.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("server exit = %v", err)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	client, err := NewClient(filepath.Join(dir, "engine.sock"))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestClientSendsReferenceWithoutResolvingIt(t *testing.T) {
	t.Setenv("SAMPLE_URL", "postgres://user:private-secret@example.invalid/sample")
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		var request Request
		if err := json.Unmarshal(body, &request); err != nil || request.APIVersion != Version || request.Action != ProfileAdd || r.Host != "forkden" {
			t.Errorf("request = %s, %v; want v1 profile registration", body, err)
		}
		var input ProfileInput
		if err := json.Unmarshal(request.Params, &input); err != nil || input.URLEnv != "SAMPLE_URL" {
			t.Errorf("profile params = %s, %v; want reference", request.Params, err)
		}
		if strings.Contains(string(body), "private-secret") {
			t.Error("CLI resolved and transmitted a source credential")
		}
		if _, err := w.Write([]byte(`{"api_version":"1","ok":true,"data":{"name":"sample","url_env":"SAMPLE_URL"}}`)); err != nil {
			t.Error(err)
		}
	}))
	if _, err := client.Execute(t.Context(), ProfileAdd, ProfileInput{Name: "sample", URLEnv: "SAMPLE_URL"}); err != nil {
		t.Fatal(err)
	}
}

func TestClientRejectsProtocolMismatchAndDoesNotRetry(t *testing.T) {
	t.Parallel()
	for _, reply := range []string{`{"api_version":"2","ok":true,"data":{}}`, `{"api_version":"1","ok":false,"data":null}`, `not JSON`} {
		t.Run(reply, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				if _, err := w.Write([]byte(reply)); err != nil {
					t.Error(err)
				}
			}))
			if _, err := client.Execute(t.Context(), ForkCreate, CreateInput{Source: "sample", TTLSeconds: 3600}); err == nil || calls.Load() != 1 {
				t.Fatalf("incompatible response = %v, calls %d; want single failed request", err, calls.Load())
			}
		})
	}
}

func TestClientNeverFollowsRedirectToTCP(t *testing.T) {
	t.Parallel()
	var externalCalls atomic.Int32
	external := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { externalCalls.Add(1) }))
	t.Cleanup(external.Close)
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, external.URL, http.StatusTemporaryRedirect)
	}))
	if _, err := client.Execute(t.Context(), ForkCreate, CreateInput{Source: "sample", TTLSeconds: 3600}); err == nil || externalCalls.Load() != 0 {
		t.Fatalf("redirect = %v, external calls %d; want rejected without external traffic", err, externalCalls.Load())
	}
}

func TestClientPropagatesCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		if _, err := w.Write([]byte(`{"api_version":"1","ok":true,"data":{}}`)); err != nil {
			return // Cancellation may close the connection before the response.
		}
	}))
	if _, err := client.Status(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request = %v; want context cancellation", err)
	}
}
