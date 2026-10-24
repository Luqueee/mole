package transfer

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const tokenBytes = 32

// TokenPath is where a machine keeps the shared secret: the daemon that
// receives remote-to-local transfers writes it, and installs a copy on the
// remote so only that user's processes there can send.
func TokenPath(stateDir string) string { return filepath.Join(stateDir, "transfer.token") }

// ValidToken reports whether s looks like a token this package generated.
func ValidToken(s string) bool {
	if len(s) != tokenBytes*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ReadToken returns the token stored at path.
func ReadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if !ValidToken(tok) {
		return "", fmt.Errorf("%s does not hold a valid transfer token", path)
	}
	return tok, nil
}

// WriteToken stores tok at path, readable by this user only.
func WriteToken(path, tok string) error {
	if !ValidToken(tok) {
		return errors.New("invalid transfer token")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".token-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(tok + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoadOrCreateToken returns the token at path, generating one first if none
// is stored.
func LoadOrCreateToken(path string) (string, error) {
	if tok, err := ReadToken(path); err == nil {
		return tok, nil
	}
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	if err := WriteToken(path, tok); err != nil {
		return "", err
	}
	return tok, nil
}

// PeerPortPath is where the remote keeps the port its peer's receiver is
// reachable on (the daemon installs it together with the token).
func PeerPortPath(stateDir string) string { return filepath.Join(stateDir, "transfer.port") }

// WritePeerPort stores the peer's port at path.
func WritePeerPort(path string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid transfer port %d", port)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(port)+"\n"), 0o600)
}

// ReadPeerPort returns the peer's port stored at path.
func ReadPeerPort(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s does not hold a valid port", path)
	}
	return port, nil
}
