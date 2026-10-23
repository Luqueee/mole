package transfer

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// File is a file ready to send.
type File struct {
	FileInfo
	Open func() (io.ReadCloser, error)
}

// FileFromPath describes the regular file at path (following symlinks).
// Directories are refused: this version sends files only.
func FileFromPath(path string) (File, error) {
	st, err := os.Stat(path)
	if err != nil {
		return File{}, err
	}
	if st.IsDir() {
		return File{}, fmt.Errorf("%s is a directory (only files can be sent)", path)
	}
	if !st.Mode().IsRegular() {
		return File{}, fmt.Errorf("%s is not a regular file", path)
	}
	return File{
		FileInfo: FileInfo{Name: filepath.Base(path), Size: st.Size(), Mode: uint32(st.Mode().Perm()), MTime: st.ModTime().Unix()},
		Open:     func() (io.ReadCloser, error) { return os.Open(path) },
	}, nil
}
