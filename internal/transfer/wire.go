// Package transfer sends files between two mole machines. Each machine has
// an inbox, the folder where files sent to it land; the sender names no
// path, only (optionally) a sub-folder of that inbox.
//
// The protocol is symmetric and independent of how the two ends are
// connected: it runs over any io.ReadWriteCloser, either an SSH exec session
// (local to remote) or a TCP connection reached through a push forward
// (remote to local).
//
//	sender   -> {"v":1,"token":"..","to":"sub","files":[{name,size,mode,mtime}...]}\n
//	receiver -> {"ok":true,"dir":"/abs/inbox/sub"}\n        (or {"ok":false,"error":".."})
//	for each file:
//	  sender   -> size raw bytes, then the 32-byte SHA-256 of those bytes
//	  receiver -> {"ok":true,"saved":"name (1)","bytes":N}\n
package transfer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	protoVersion = 1
	// maxLineBytes bounds a JSON line: a hello naming maxFiles files fits.
	maxLineBytes = 4 << 20
	maxFiles     = 10000
	// maxFileSize keeps size arithmetic far from overflow.
	maxFileSize = 1 << 50
)

// FileInfo describes one file in a transfer.
type FileInfo struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Mode  uint32 `json:"mode"`
	MTime int64  `json:"mtime"` // unix seconds
}

type hello struct {
	V     int        `json:"v"`
	Token string     `json:"token,omitempty"`
	To    string     `json:"to,omitempty"`
	Files []FileInfo `json:"files"`
}

type reply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Dir   string `json:"dir,omitempty"`
	Saved string `json:"saved,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`
}

// RemoteError is a refusal or failure reported by the other end.
type RemoteError struct{ Msg string }

func (e *RemoteError) Error() string { return "the other side refused: " + e.Msg }

// BadReplyError means the peer answered with something that is not this
// protocol, typically a mole too old to know `recv`, or a shell error.
type BadReplyError struct{ Line string }

func (e *BadReplyError) Error() string {
	return fmt.Sprintf("unexpected reply from the other side: %q (is mole up to date there?)", e.Line)
}

var errLineTooLong = errors.New("transfer: line too long")

func writeJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxLineBytes {
			return nil, errLineTooLong
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

// maxBlankLines is how many empty lines readJSONLine skips before giving up:
// the protocol never sends any, but a program that is not mole (a shell
// banner, an old mole printing its usage) may start with some.
const maxBlankLines = 8

func readJSONLine(br *bufio.Reader, v any) error {
	var line []byte
	for i := 0; ; i++ {
		var err error
		if line, err = readLine(br); err != nil {
			return err
		}
		if len(bytes.TrimSpace(line)) > 0 {
			break
		}
		if i >= maxBlankLines {
			return &BadReplyError{Line: ""}
		}
	}
	if err := json.Unmarshal(line, v); err != nil {
		text := strings.TrimSpace(string(line))
		if len(text) > 200 {
			text = text[:200]
		}
		return &BadReplyError{Line: text}
	}
	return nil
}
