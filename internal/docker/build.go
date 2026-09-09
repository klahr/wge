package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Build builds an image from a tar build context and tags it.
//
// Progress arrives as a stream of newline-delimited JSON objects; a failure is
// reported inside that stream rather than by the HTTP status, so the body has
// to be read to the end before a build can be called successful.
func (d *Client) Build(ctx context.Context, tag string, context io.Reader, progress io.Writer) error {
	q := url.Values{
		"t":          {tag},
		"dockerfile": {"Dockerfile"},
		"rm":         {"true"},
		"forcerm":    {"true"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url("/build?"+q.Encode()), context)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")

	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newAPIError(resp)
	}
	return drainBuildOutput(resp.Body, progress)
}

type buildMessage struct {
	Stream string `json:"stream"`
	Error  string `json:"error"`
}

func drainBuildOutput(r io.Reader, progress io.Writer) error {
	dec := json.NewDecoder(r)
	var failure string
	var tail []string

	for {
		var msg buildMessage
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("read build output: %w", err)
		}

		if msg.Stream != "" {
			if progress != nil {
				_, _ = io.WriteString(progress, msg.Stream)
			}
			// Keep a short tail; a build failure's cause is almost always in
			// the last few lines the daemon printed, not in the error field.
			if line := strings.TrimRight(msg.Stream, "\n"); line != "" {
				tail = append(tail, line)
				if len(tail) > 12 {
					tail = tail[1:]
				}
			}
		}
		if msg.Error != "" {
			failure = msg.Error
		}
	}

	if failure != "" {
		return fmt.Errorf("build failed: %s\n%s", failure, strings.Join(tail, "\n"))
	}
	return nil
}

// PutArchive extracts a tar into a container's filesystem at path.
//
// This is how a run's rendered files reach its container: the engine writes the
// tar, so ownership, permissions and mtimes are set exactly as intended, and
// the container needs no tooling of its own to receive them.
func (d *Client) PutArchive(ctx context.Context, container, path string, archive io.Reader) error {
	q := url.Values{"path": {path}}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		d.url("/containers/"+container+"/archive?"+q.Encode()), archive)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")

	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("upload archive: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newAPIError(resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// ExecOptions configure a one-shot command in a running container.
type ExecOptions struct {
	Cmd   []string
	User  string
	Env   []string
	Stdin io.Reader
}

// ExecResult is the outcome of a one-shot command.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Exec runs a command to completion and collects its output. It is the
// non-interactive counterpart to the runtime's attach path, used for the
// handful of things the engine does to a container itself.
func (d *Client) Exec(ctx context.Context, container string, opts ExecOptions) (*ExecResult, error) {
	create := map[string]any{
		"AttachStdin":  opts.Stdin != nil,
		"AttachStdout": true,
		"AttachStderr": true,
		"Tty":          false,
		"User":         opts.User,
		"Env":          opts.Env,
		"Cmd":          opts.Cmd,
	}

	var created struct {
		ID string `json:"Id"`
	}
	if err := d.Post(ctx, "/containers/"+container+"/exec", create, &created); err != nil {
		return nil, fmt.Errorf("create exec: %w", err)
	}

	conn, br, err := d.Hijack(ctx, "/exec/"+created.ID+"/start", map[string]any{
		"Detach": false,
		"Tty":    false,
	})
	if err != nil {
		return nil, fmt.Errorf("start exec: %w", err)
	}
	defer conn.Close()

	if opts.Stdin != nil {
		if _, err := io.Copy(conn, opts.Stdin); err != nil {
			return nil, fmt.Errorf("write stdin: %w", err)
		}
	}
	// The command must see EOF or anything reading stdin will hang forever.
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}

	var stdout, stderr bytes.Buffer
	if err := demultiplex(br, &stdout, &stderr); err != nil {
		return nil, fmt.Errorf("read output: %w", err)
	}

	var inspect struct {
		ExitCode int `json:"ExitCode"`
	}
	if err := d.Get(ctx, "/exec/"+created.ID+"/json", &inspect); err != nil {
		return nil, err
	}

	return &ExecResult{
		ExitCode: inspect.ExitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

// streamHeader is the framing Docker applies when an exec has no TTY: one byte
// of stream id, three reserved, then a big-endian payload length.
const streamHeader = 8

func demultiplex(r io.Reader, stdout, stderr io.Writer) error {
	header := make([]byte, streamHeader)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}

		w := stdout
		if header[0] == 2 {
			w = stderr
		}
		if _, err := io.CopyN(w, r, int64(binary.BigEndian.Uint32(header[4:]))); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// RemoveImage deletes an image by name or id.
func (d *Client) RemoveImage(ctx context.Context, name string) error {
	return d.Delete(ctx, "/images/"+url.PathEscape(name)+"?force=true")
}

// Container is a summary of one container, as the engine lists it.
type Container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

// Name returns the container's name without the leading slash the engine adds.
func (c Container) Name() string {
	if len(c.Names) == 0 {
		return ""
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// ListContainers returns every container carrying the given label, running or
// not. It is how the engine finds what a previous process left behind.
func (d *Client) ListContainers(ctx context.Context, label string) ([]Container, error) {
	filters, err := json.Marshal(map[string][]string{"label": {label}})
	if err != nil {
		return nil, err
	}

	q := url.Values{"all": {"true"}, "filters": {string(filters)}}

	var out []Container
	if err := d.Get(ctx, "/containers/json?"+q.Encode(), &out); err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	return out, nil
}
