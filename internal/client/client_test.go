package client

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestConnectionRefusedProvidesApplicableRecoveryAndPreservesCause(t *testing.T) {
	for _, test := range []struct {
		endpoint string
		local    bool
	}{
		{"http://127.0.0.1:8765", true},
		{"http://[::1]:8765", true},
		{"http://localhost:8765", true},
		{"http://LOCALHOST.:8765", true},
		{"https://knowledge.example.com", false},
		{"http://192.0.2.10:8765", false},
	} {
		t.Run(test.endpoint, func(t *testing.T) {
			c, err := New(test.endpoint, "secret-token")
			if err != nil {
				t.Fatal(err)
			}
			c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Idempotency-Key") != "retry-this-write" {
					t.Error("lost write request ID")
				}
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
			})
			_, err = c.Do(context.Background(), http.MethodPost, "/v1/records", nil, map[string]string{"kind": "task", "title": "Continue"}, "retry-this-write")
			if !errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatalf("lost original connection-refused error: %v", err)
			}
			if !strings.Contains(err.Error(), test.endpoint) || !strings.Contains(err.Error(), "check --url") {
				t.Fatalf("missing endpoint or recovery guidance: %v", err)
			}
			if test.local {
				if !strings.Contains(err.Error(), "start herma serve in another terminal") {
					t.Fatalf("missing local startup guidance: %v", err)
				}
			} else if strings.Contains(err.Error(), "herma serve") || !strings.Contains(err.Error(), "remote server") {
				t.Fatalf("inapplicable remote recovery guidance: %v", err)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Fatal("error exposed credentials")
			}
		})
	}
}

func TestOtherTransportErrorsKeepOriginalDiagnostic(t *testing.T) {
	c, err := New("http://127.0.0.1:8765", "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})
	_, err = c.Do(context.Background(), http.MethodGet, "/v1/schema", nil, nil, "")
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "herma serve") {
		t.Fatalf("changed unrelated transport error: %v", err)
	}
}

func TestRequestAuthenticationAndErrorEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			t.Error("missing bearer token")
		}
		if r.Header.Get("Idempotency-Key") != "request-42" {
			t.Error("missing request key")
		}
		if r.URL.Query().Get("q") != "a & b" {
			t.Error("query was not encoded")
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"record version changed"}}`))
	}))
	defer server.Close()
	c, err := New(server.URL, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Do(context.Background(), http.MethodPatch, "/v1/records/id", url.Values{"q": {"a & b"}}, map[string]int{"version": 1}, "request-42")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 409 || apiErr.Code != "conflict" || !strings.Contains(err.Error(), "record version changed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRedirectNeverReceivesCredentials(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; _, _ = w.Write([]byte(`{}`)) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	c, err := New(source.URL, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Do(context.Background(), http.MethodGet, "/v1/schema", nil, nil, "")
	if err == nil {
		t.Fatal("redirect unexpectedly succeeded")
	}
	if called {
		t.Fatal("followed a redirect with credentials")
	}
}

func TestRejectInvalidURLsAndResponses(t *testing.T) {
	for _, endpoint := range []string{"", "localhost:8765", "http://:8765", "ftp://example.com", "https://user:secret@example.com", "https://example.com?token=secret", "https://example.com?", "https://example.com/#secret"} {
		if _, err := New(endpoint, "token"); err == nil {
			t.Errorf("accepted %q", endpoint)
		}
	}
	for _, body := range []string{"not json", strings.Repeat(" ", maxResponseBytes+1)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		c, err := New(server.URL, "token")
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Do(context.Background(), http.MethodGet, "/v1/schema", nil, nil, "")
		server.Close()
		if err == nil {
			t.Fatal("accepted invalid or oversized response")
		}
	}
}

func TestSocketClientTalksOverUnixSocketAndReportsMissingServer(t *testing.T) {
	dir, err := os.MkdirTemp("", "herma")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "herma.sock")
	missing, err := NewSocket(path, "socket-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missing.Do(context.Background(), http.MethodGet, "/v1/whoami", nil, nil, ""); err == nil || !strings.Contains(err.Error(), "not listening on socket "+path) {
		t.Fatalf("missing server error: %v", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer socket-token" {
			t.Error("socket request lacked its bearer token")
		}
		_, _ = w.Write([]byte(`{"listener":"socket"}`))
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	data, err := missing.Do(context.Background(), http.MethodGet, "/v1/whoami", nil, nil, "")
	if err != nil || string(data) != `{"listener":"socket"}` {
		t.Fatalf("socket request: %s %v", data, err)
	}
	if _, err := NewSocket("relative.sock", "socket-token"); err == nil {
		t.Fatal("accepted a relative socket path")
	}
}

func TestTextReturnsNonJSONBodiesAndKeepsErrorEnvelopes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("missing credentials")
		}
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"no such thing"}}`)
			return
		}
		_, _ = io.WriteString(w, "herma context · project\n")
	}))
	defer server.Close()
	c, err := New(server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	data, err := c.Text(context.Background(), "/v1/context", url.Values{"format": {"text"}})
	if err != nil || string(data) != "herma context · project\n" {
		t.Fatalf("Text = %q, %v", data, err)
	}
	var apiErr *APIError
	if _, err := c.Text(context.Background(), "/missing", nil); !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound || apiErr.Code != "not_found" {
		t.Fatalf("error envelope lost: %v", err)
	}
}
