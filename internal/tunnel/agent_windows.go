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
// The caller selects the pipe from IdentityAgent or SSH_AUTH_SOCK. An empty
// path falls back to the OpenSSH default so Windows can use the running
// agent service without an explicit configuration.
func dialAgent(sock string) (net.Conn, error) {
	if sock == "" {
		sock = defaultWindowsAgentPipe
	}
	return winio.DialPipe(sock, nil)
}
