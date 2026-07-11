package spclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	librespot "github.com/devgianlu/go-librespot"
)

func newTestSpclient(t *testing.T, srv *httptest.Server) *Spclient {
	t.Helper()

	baseUrl, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatalf("failed parsing test server url: %v", err)
	}

	return &Spclient{
		log:         &librespot.NullLogger{},
		client:      srv.Client(),
		baseUrl:     baseUrl,
		clientToken: "client-token",
		deviceId:    "device-id",
		accessToken: func(context.Context, bool) (string, error) { return "access-token", nil },
	}
}

// TestInnerRequestResendsBodyOnRetry reproduces the retry body loss behind
// upstream issue #300: the request body was armed once outside the retry
// closure, so any retried attempt (401 token refresh, 5xx, network error) was
// sent with an empty body and the server rejected it with "400 Missing
// payload". Every attempt must carry the full payload.
func TestInnerRequestResendsBodyOnRetry(t *testing.T) {
	payload := []byte("proto-payload-bytes")

	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed reading request body: %v", err)
		}

		mu.Lock()
		bodies = append(bodies, body)
		attempt := len(bodies)
		mu.Unlock()

		if attempt == 1 {
			// Fail the first attempt so innerRequest retries it.
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestSpclient(t, srv)

	resp, err := c.innerRequest(context.Background(), "PUT", c.baseUrl.JoinPath("test"), nil, nil, payload)
	if err != nil {
		t.Fatalf("innerRequest failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status code: %d", resp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(bodies) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(bodies))
	}
	for i, body := range bodies {
		if string(body) != string(payload) {
			t.Errorf("attempt %d sent body %q, want %q", i+1, body, payload)
		}
	}
}

// TestInnerRequestResendsBodyOnUnauthorized covers the 401 path: the token is
// refreshed and the request retried, which must also carry the full body.
func TestInnerRequestResendsBodyOnUnauthorized(t *testing.T) {
	payload := []byte("proto-payload-bytes")

	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed reading request body: %v", err)
		}

		mu.Lock()
		bodies = append(bodies, body)
		attempt := len(bodies)
		mu.Unlock()

		if attempt == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestSpclient(t, srv)

	resp, err := c.innerRequest(context.Background(), "POST", c.baseUrl.JoinPath("test"), nil, nil, payload)
	if err != nil {
		t.Fatalf("innerRequest failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	mu.Lock()
	defer mu.Unlock()

	if len(bodies) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(bodies))
	}
	for i, body := range bodies {
		if string(body) != string(payload) {
			t.Errorf("attempt %d sent body %q, want %q", i+1, body, payload)
		}
	}
}
