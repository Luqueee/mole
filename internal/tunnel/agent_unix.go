//go:build !windows

package tunnel

import (
	"errors"
	"net"
)

// dialAgent connects to the user's SSH agent via a Unix domain socket
// at the path given by SSH_AUTH_SOCK.
//
// On non-Windows platforms the SSH agent is always exposed as a Unix
// socket; if SSH_AUTH_SOCK is empty the agent is simply unavailable
// and a "not set" error is returned so the caller can skip it and
// fall through to direct key files.
func dialAgent(sock string) (net.Conn, error) {
	if sock == "" {
		return nil, errors.New("SSH_AUTH_SOCK not set")
	}
	return net.Dial("unix", sock)
}
