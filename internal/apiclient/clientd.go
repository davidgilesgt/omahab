package apiclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/client"
)

// ClientdClient talks to omahab-clientd over a Unix domain socket via NDJSON.
// The shell plugin (QML) also talks to this daemon; the CLI delegates
// desktop actions there when available. Wire format is SocketRequest/SocketResponse
// (newline-delimited JSON) with canonical method names defined in companion/PROTOCOL.md.
type ClientdClient struct {
	SocketPath string
}

// DefaultClientdSocketPath returns the Unix socket path.
// Single canonical implementation lives in internal/client/config.go.
func DefaultClientdSocketPath() string {
	return client.DefaultSocketPath()
}

// NewClientdClient creates a client bound to socketPath.
func NewClientdClient(socketPath string) *ClientdClient {
	if socketPath == "" {
		socketPath = DefaultClientdSocketPath()
	}
	return &ClientdClient{SocketPath: socketPath}
}

// Available reports whether the socket exists and is dialable.
func (c *ClientdClient) Available(ctx context.Context) bool {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ClientdError is the typed error returned when the daemon replies with {"error":{"code":...}}.
type ClientdError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ClientdError) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

// Call performs a typed NDJSON call: {"id":..., "method":..., "params":...} -> {"id":..., "result":..., "error":...}.
// out may be nil; when non-nil and the daemon returns a result, it is JSON-decoded into out.
func (c *ClientdClient) Call(ctx context.Context, method string, params map[string]any, out any) error {
	method = strings.TrimSpace(method)
	if method == "" {
		return fmt.Errorf("method required")
	}
	// Generate simple request id.
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	req := client.SocketRequest{ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Context-aware deadlines.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	}

	if _, err := conn.Write(append(data, '\n')); err != nil {
		return err
	}

	br := bufio.NewReader(conn)
	var buf strings.Builder
	tmp := make([]byte, 4096)
	for {
		// Respect context cancellation while reading.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, rerr := br.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			trimmed := strings.TrimSpace(buf.String())
			if trimmed != "" && json.Valid([]byte(trimmed)) && br.Buffered() == 0 {
				break
			}
			if buf.Len() > 1<<20 {
				return fmt.Errorf("response too large")
			}
		}
		if rerr != nil {
			break
		}
	}
	raw := strings.TrimSpace(buf.String())
	if raw == "" {
		return fmt.Errorf("empty response from daemon")
	}
	var resp client.SocketResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return fmt.Errorf("invalid daemon response: %w: %s", err, raw)
	}
	if resp.Error != nil {
		return &ClientdError{Code: resp.Error.Code, Message: resp.Error.Message}
	}
	if out != nil && resp.Result != nil {
		// Re-marshal Result then unmarshal into out to handle any destination type.
		b, err := json.Marshal(resp.Result)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, out); err != nil {
			return err
		}
	}
	return nil
}
