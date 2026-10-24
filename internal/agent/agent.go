// Package agent is the control protocol a mole node speaks to another node
// that manages it: one JSON request per line in, one JSON response per line
// out, over any byte stream (in practice an SSH exec session running
// `mole agent --stdio`, so SSH is the authentication).
//
// The set of operations is closed and versioned. A node that does not know an
// operation says so instead of failing, which lets nodes of different
// versions coexist.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
)

// ProtocolVersion is bumped when the meaning of an existing operation changes.
const ProtocolVersion = 1

// Operations.
const (
	OpHello  = "hello"
	OpStatus = "status"
)

// maxLine bounds one request or response line.
const maxLine = 4 << 20

// Request is one call.
type Request struct {
	ID     int             `json:"id"`
	Op     string          `json:"op"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response answers a Request with the same ID.
type Response struct {
	ID     int             `json:"id"`
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// Hello identifies a node and what it can do.
type Hello struct {
	Agent    int      `json:"agent"` // ProtocolVersion
	Version  string   `json:"version"`
	Build    string   `json:"build,omitempty"` // where the binary was built from, e.g. "feat/x@a1b2c3d"
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	Hostname string   `json:"hostname"`
	Caps     []string `json:"caps"` // operations this node implements
}

// Status is what a node reports about itself.
type Status struct {
	// Running is whether a daemon (mole up) answers on this node.
	Running    bool   `json:"running"`
	Remote     string `json:"remote,omitempty"` // the node's own `remote:`
	AdminAddr  string `json:"admin_addr,omitempty"`
	ConfigPath string `json:"config_path,omitempty"`
	Inbox      string `json:"inbox,omitempty"`
	Nodes      int    `json:"nodes"` // how many nodes it knows
	// The node's own port configuration (what its daemon would forward).
	AutoDiscover  bool  `json:"auto_discover,omitempty"`
	DiscoverPorts []int `json:"discover_ports,omitempty"`
	ExcludePorts  []int `json:"exclude_ports,omitempty"`
	// Admin is the daemon's own /status document, verbatim, when it runs.
	Admin json.RawMessage `json:"admin,omitempty"`
}

// Handler implements the operations on a node.
type Handler interface {
	Hello() Hello
	Status() (Status, error)
}

// Serve answers requests on rw until it reaches end of input or ctx ends.
func Serve(ctx context.Context, rw io.ReadWriter, h Handler) error {
	br := bufio.NewReaderSize(rw, 64<<10)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := readLine(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var req Request
		var resp Response
		if err := json.Unmarshal(line, &req); err != nil {
			resp = Response{Error: "malformed request"}
		} else {
			resp = handle(h, req)
		}
		b, _ := json.Marshal(resp)
		if _, err := rw.Write(append(b, '\n')); err != nil {
			return err
		}
	}
}

func handle(h Handler, req Request) Response {
	resp := Response{ID: req.ID}
	var result any
	switch req.Op {
	case OpHello:
		result = h.Hello()
	case OpStatus:
		st, err := h.Status()
		if err != nil {
			resp.Error = err.Error()
			return resp
		}
		result = st
	default:
		resp.Error = fmt.Sprintf("unsupported op %q", req.Op)
		return resp
	}
	b, err := json.Marshal(result)
	if err != nil {
		resp.Error = err.Error()
		return resp
	}
	resp.OK, resp.Result = true, b
	return resp
}

func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxLine {
			return nil, errors.New("agent: line too long")
		}
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

// isGone reports whether err means the other end of the stream has closed.
func isGone(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.EPIPE)
}

// ErrNotAgent means the other end answered with something that is not this
// protocol: a mole too old to have an agent, or a shell error.
type ErrNotAgent struct{ Output string }

func (e *ErrNotAgent) Error() string {
	return fmt.Sprintf("the node does not speak the agent protocol (it said: %q)", e.Output)
}

// RemoteError is an error the node reported for a call.
type RemoteError struct{ Msg string }

func (e *RemoteError) Error() string { return e.Msg }

// Client calls a node's agent. Calls are serialised.
type Client struct {
	rw   io.ReadWriteCloser
	br   *bufio.Reader
	mu   sync.Mutex
	next int
}

// NewClient wraps a connection to a node's agent. Close closes it.
func NewClient(rw io.ReadWriteCloser) *Client {
	return &Client{rw: rw, br: bufio.NewReaderSize(rw, 64<<10)}
}

// Close closes the connection.
func (c *Client) Close() error { return c.rw.Close() }

func (c *Client) call(ctx context.Context, op string, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = c.rw.Close() })
	defer stop()

	c.next++
	b, _ := json.Marshal(Request{ID: c.next, Op: op})
	if _, err := c.rw.Write(append(b, '\n')); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if isGone(err) {
			return &ErrNotAgent{} // the process ended before reading anything
		}
		return err
	}
	var line []byte
	for { // the protocol sends no blank lines; a banner may
		var err error
		if line, err = readLine(c.br); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return &ErrNotAgent{}
			}
			return err
		}
		if len(strings.TrimSpace(string(line))) > 0 {
			break
		}
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil || resp.ID != c.next {
		text := strings.TrimSpace(string(line))
		if len(text) > 200 {
			text = text[:200]
		}
		return &ErrNotAgent{Output: text}
	}
	if !resp.OK {
		return &RemoteError{Msg: resp.Error}
	}
	if out != nil {
		return json.Unmarshal(resp.Result, out)
	}
	return nil
}

// Hello asks the node who it is.
func (c *Client) Hello(ctx context.Context) (Hello, error) {
	var h Hello
	err := c.call(ctx, OpHello, &h)
	return h, err
}

// Status asks the node how it is doing.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	err := c.call(ctx, OpStatus, &s)
	return s, err
}
