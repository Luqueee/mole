package tunnel

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// defaultIdentityNames are the key files mole tries under ~/.ssh after the
// agent, in this order.
var defaultIdentityNames = []string{"id_ed25519", "id_rsa", "id_ecdsa", "id_dsa"}

// sshDefaultIdentityNames are the files OpenSSH lists as IdentityFile when
// ssh_config configures none; `ssh -G` prints them for every such host.
var sshDefaultIdentityNames = []string{
	"id_rsa", "id_ecdsa", "id_ecdsa_sk", "id_ed25519", "id_ed25519_sk", "id_xmss", "id_dsa",
}

// identitySigners returns every key mole offers for remote, in the order
// they are tried, and the agent connection the agent-backed signers use.
// The connection is nil when no agent signer is returned; otherwise the
// caller owns it and must keep it open while the signers are in use.
//
// The order is: IdentityFile entries, then agent keys, then the default
// ~/.ssh/id_* files. A key is offered once even if both a file and the
// agent hold it. An encrypted file whose public key is in the agent is
// offered through the agent at the file's position. Servers count every
// rejected key against MaxAuthTries (6 by default), so the keys the user
// configured come first.
//
// When IdentitiesOnly is set, only the IdentityFile entries are offered
// (agent keys only when they match one of them), as with OpenSSH.
func identitySigners(remote Remote, log *slog.Logger) ([]ssh.Signer, net.Conn, error) {
	home, _ := os.UserHomeDir()
	explicit, defaults := identityFileOrder(remote, home)

	agentSigners, agentConn := agentIdentities(remote.IdentityAgent, log)
	agentByKey := make(map[string]ssh.Signer, len(agentSigners))
	for _, s := range agentSigners {
		agentByKey[string(s.PublicKey().Marshal())] = s
	}

	var signers []ssh.Signer
	seen := make(map[string]bool)
	usedAgent := false
	add := func(s ssh.Signer, fromAgent bool) {
		k := string(s.PublicKey().Marshal())
		if seen[k] {
			return
		}
		seen[k] = true
		signers = append(signers, s)
		usedAgent = usedAgent || fromAgent
	}

	var encrypted []string
	addFile := func(path string) {
		signer, public, err := loadIdentityFile(path)
		if signer != nil {
			add(signer, false)
			return
		}
		if public != nil {
			if s, ok := agentByKey[string(public.Marshal())]; ok {
				add(s, true)
				return
			}
		}
		var missing *ssh.PassphraseMissingError
		switch {
		case errors.As(err, &missing):
			encrypted = append(encrypted, path)
			log.Warn("skipping passphrase-protected SSH key that is not in ssh-agent; mole cannot prompt for passphrases, add it with ssh-add",
				"key", path)
		case errors.Is(err, fs.ErrNotExist):
			// ssh -G lists the default files whether or not they exist.
		default:
			log.Debug("skipping unusable SSH key file", "key", path, "err", err)
		}
	}

	for _, path := range explicit {
		addFile(path)
	}
	if !remote.IdentitiesOnly {
		for _, s := range agentSigners {
			add(s, true)
		}
	}
	for _, path := range defaults {
		addFile(path)
	}

	if !usedAgent && agentConn != nil {
		_ = agentConn.Close()
		agentConn = nil
	}
	if len(signers) == 0 {
		msg := "no SSH auth methods: set up ssh-agent or ~/.ssh/id_*"
		if len(encrypted) > 0 {
			msg += fmt.Sprintf(" (passphrase-protected keys must be added to ssh-agent with ssh-add: %s)", strings.Join(encrypted, ", "))
		}
		return nil, nil, errors.New(msg)
	}
	return signers, agentConn, nil
}

// identityFileOrder splits the key files into those tried before the agent
// (explicit) and after it (defaults).
//
// `ssh -G` prints OpenSSH's default IdentityFile list when ssh_config sets
// none. Those are treated as defaults, so an unauthorized ~/.ssh/id_rsa
// does not use up a server's MaxAuthTries ahead of the agent.
func identityFileOrder(remote Remote, home string) (explicit, defaults []string) {
	if remote.IdentitiesOnly {
		return remote.IdentityFiles, nil
	}
	explicit = remote.IdentityFiles
	if onlySSHDefaultIdentities(explicit, home) {
		explicit = nil
	}
	if home != "" {
		for _, name := range defaultIdentityNames {
			defaults = append(defaults, filepath.Join(home, ".ssh", name))
		}
	}
	return explicit, defaults
}

func onlySSHDefaultIdentities(paths []string, home string) bool {
	if len(paths) == 0 || home == "" {
		return false
	}
	for _, p := range paths {
		isDefault := false
		for _, name := range sshDefaultIdentityNames {
			if filepath.Clean(p) == filepath.Join(home, ".ssh", name) {
				isDefault = true
				break
			}
		}
		if !isDefault {
			return false
		}
	}
	return true
}

// loadIdentityFile parses a private key file. For a passphrase-protected
// key it returns the public key when known (embedded in the OpenSSH format,
// or from path.pub) with the *ssh.PassphraseMissingError, so an agent copy
// of the key can be used instead.
func loadIdentityFile(path string) (ssh.Signer, ssh.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err == nil {
		return signer, signer.PublicKey(), nil
	}
	var missing *ssh.PassphraseMissingError
	if !errors.As(err, &missing) {
		return nil, nil, err
	}
	public := missing.PublicKey
	if public == nil {
		if data, readErr := os.ReadFile(path + ".pub"); readErr == nil {
			if key, _, _, _, parseErr := ssh.ParseAuthorizedKey(data); parseErr == nil {
				public = key
			}
		}
	}
	return nil, public, err
}

// agentIdentities connects to the agent selected by identityAgent and
// returns its signers with the connection they use. It returns no
// connection when the agent is disabled, unreachable, or holds no keys.
func agentIdentities(identityAgent string, log *slog.Logger) ([]ssh.Signer, net.Conn) {
	// A host's IdentityAgent overrides the inherited agent socket. An absent
	// directive retains the platform default, including Windows' OpenSSH pipe.
	sock := os.Getenv("SSH_AUTH_SOCK")
	switch {
	case strings.EqualFold(identityAgent, "none"):
		return nil, nil
	case identityAgent == "SSH_AUTH_SOCK":
		if sock == "" {
			return nil, nil
		}
	case identityAgent != "":
		sock = expandHome(os.ExpandEnv(identityAgent))
		if sock == "" {
			return nil, nil
		}
	}
	conn, err := dialAgentFunc(sock)
	if err != nil {
		log.Debug("ssh-agent unavailable", "err", err)
		return nil, nil
	}
	signers, err := agent.NewClient(conn).Signers()
	if err != nil || len(signers) == 0 {
		if err != nil {
			log.Debug("ssh-agent keys unavailable", "err", err)
		}
		_ = conn.Close()
		return nil, nil
	}
	return signers, conn
}
