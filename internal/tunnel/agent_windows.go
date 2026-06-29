//go:build windows

package tunnel

import (
	"net"

	"github.com/Microsoft/go-winio"
)

// defaultWindowsAgentPipe is the named pipe used by the OpenSSH
// agent on Windows (set up by `Start-Service ssh-agent` or by the
// user's ssh-agent setup).
const defaultWindowsAgentPipe = `\\.\pipe\openssh-ssh-agent`

// dialAgent connects to the user's SSH agent via a Windows named pipe.
// The pipe path comes from SSH_AUTH_SOCK; if it is not set we fall
// back to the OpenSSH default so that `mole` works out of the box on
// Windows when the agent service is running.
func dialAgent(sock string) (net.Conn, error) {
	if sock == "" {
		sock = defaultWindowsAgentPipe
	}
	return winio.DialPipe(sock, nil)
}