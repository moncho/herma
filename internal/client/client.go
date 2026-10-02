// Package client provides the authenticated HTTP transport used by herma.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const maxResponseBytes = 16 << 20

type Client struct {
	baseURL string
	token   string
	socket  string
	http    *http.Client
}

// NewSocket connects through a local Unix domain socket. Reviewer identities
// use it because the server accepts reviewer tokens only there.
func NewSocket(path, token string) (*Client, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("socket path must be absolute")
	}
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("a nonempty bearer token is required")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", path)
	}}
	return &Client{baseURL: "http://herma.socket", token: token, socket: path, http: &http.Client{
		Timeout:       30 * time.Second,
		Transport:     transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// New never forwards credentials through a redirect. URL credentials, query
// parameters and fragments are rejected so that authentication has one source.
func New(baseURL, token string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("URL must be an absolute http or https URL without credentials, a query, or a fragment")
	}
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("a nonempty bearer token is required")
	}
	return &Client{baseURL: strings.TrimRight(u.String(), "/"), token: token, http: &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("server returned HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("%s (HTTP %d): %s", e.Code, e.Status, e.Message)
}

func (c *Client) Do(ctx context.Context, method, path string, query url.Values, input any, requestID string) (json.RawMessage, error) {
	data, err := c.do(ctx, method, path, query, input, requestID, "application/json")
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) {
		return nil, errors.New("server returned an invalid JSON response")
	}
	return json.RawMessage(data), nil
}

// Text performs a GET for a text response, such as the compact context or the
// rendered principles file. Errors still use the server's JSON error envelope.
func (c *Client) Text(ctx context.Context, path string, query url.Values) ([]byte, error) {
	data, err := c.do(ctx, http.MethodGet, path, query, nil, "", "text/plain, text/markdown")
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, errors.New("server returned an invalid UTF-8 response")
	}
	return data, nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, input any, requestID, accept string) ([]byte, error) {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("prepare request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", accept)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if requestID != "" {
		req.Header.Set("Idempotency-Key", requestID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if c.socket != "" && (errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)) {
			return nil, fmt.Errorf("knowledge base server is not listening on socket %s; start herma serve on this machine: %w", c.socket, err)
		}
		if errors.Is(err, syscall.ECONNREFUSED) {
			host := strings.TrimSuffix(req.URL.Hostname(), ".")
			if strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback() {
				return nil, fmt.Errorf("knowledge base server is not running or reachable at %s; start herma serve in another terminal and check --url: %w", c.baseURL, err)
			}
			return nil, fmt.Errorf("knowledge base server is not reachable at %s; check --url and that the remote server is running: %w", c.baseURL, err)
		}
		return nil, fmt.Errorf("contact knowledge base: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read server response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return nil, errors.New("server response exceeds the 16 MiB client limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &envelope)
		if envelope.Error.Message == "" {
			envelope.Error.Message = http.StatusText(resp.StatusCode)
		}
		return nil, &APIError{Status: resp.StatusCode, Code: envelope.Error.Code, Message: envelope.Error.Message}
	}
	return data, nil
}
