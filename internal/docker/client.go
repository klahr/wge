package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// APIVersion is pinned so an engine upgrade cannot silently change the shape
// of a response underneath us.
const APIVersion = "v1.43"

// DefaultSocket is the Docker Engine's unix socket.
const DefaultSocket = "/var/run/docker.sock"

// APIError is a non-2xx response from the Docker Engine.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("docker api: %d: %s", e.Status, e.Message)
}

// IsNotFound reports whether err is a 404 from the engine.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// Client talks to the Docker Engine over its unix socket.
//
// Only a handful of endpoints are needed -- build, create, start, exec, upload,
// remove -- which is a poor trade for the official SDK's dependency tree.
type Client struct {
	address string
	network string
	http    *http.Client
}

// New returns a client for the engine at endpoint.
//
// An endpoint is a unix socket path, a unix:// URL, or tcp://host:port. The
// last is what makes more than one machine possible: a pool whose members are
// all local sockets is a pool of one wearing several names.
func New(endpoint string) *Client {
	if endpoint == "" {
		endpoint = DefaultSocket
	}

	network, address := "unix", endpoint
	switch {
	case strings.HasPrefix(endpoint, "unix://"):
		address = strings.TrimPrefix(endpoint, "unix://")
	case strings.HasPrefix(endpoint, "tcp://"):
		network, address = "tcp", strings.TrimPrefix(endpoint, "tcp://")
	case strings.HasPrefix(endpoint, "http://"):
		network, address = "tcp", strings.TrimPrefix(endpoint, "http://")
	}

	return &Client{
		address: address,
		network: network,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, address)
				},
			},
		},
	}
}

// Endpoint is the address this client dials: a socket path or host:port.
func (d *Client) Endpoint() string { return d.address }

func (d *Client) url(path string) string {
	// The host is ignored -- the transport always dials the unix socket -- but
	// net/http requires a syntactically valid URL.
	return "http://docker/" + APIVersion + path
}

// Get performs a GET and decodes the JSON response into out.
func (d *Client) Get(ctx context.Context, path string, out any) error {
	return d.roundTrip(ctx, http.MethodGet, path, nil, out)
}

// Post performs a POST with an optional JSON body and response.
func (d *Client) Post(ctx context.Context, path string, body, out any) error {
	return d.roundTrip(ctx, http.MethodPost, path, body, out)
}

// Delete performs a DELETE.
func (d *Client) Delete(ctx context.Context, path string) error {
	return d.roundTrip(ctx, http.MethodDelete, path, nil, nil)
}

func (d *Client) roundTrip(ctx context.Context, method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, d.url(path), payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newAPIError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func newAPIError(resp *http.Response) error {
	var payload struct {
		Message string `json:"message"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Message == "" {
		payload.Message = strings.TrimSpace(string(raw))
	}
	return &APIError{Status: resp.StatusCode, Message: payload.Message}
}

// hijack starts an exec and takes over the raw connection.
//
// Attaching to a process needs a bidirectional byte stream, which net/http will
// not surrender from a normal response, so the request is written by hand and
// the connection is kept. The returned reader holds whatever the engine had
// already buffered and must be read instead of the connection itself.
// Hijack starts a request and takes over the raw connection.
func (d *Client) Hijack(ctx context.Context, path string, body any) (net.Conn, *bufio.Reader, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("encode request: %w", err)
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, d.network, d.address)
	if err != nil {
		return nil, nil, fmt.Errorf("dial docker engine at %s: %w", d.address, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url(path), bytes.NewReader(encoded))
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	req.Host = "docker"

	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("write hijack request: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("read hijack response: %w", err)
	}

	// 101 is the upgrade; 200 happens when the engine declines to upgrade but
	// still streams, which is equally usable.
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		err := newAPIError(resp)
		resp.Body.Close()
		conn.Close()
		return nil, nil, err
	}

	// Cancelling the context must drop the stream; nothing else will interrupt
	// a blocked read on a hijacked connection.
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	return conn, br, nil
}
