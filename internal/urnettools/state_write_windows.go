//go:build windows

package urnettools

import (
	"os"
	"path/filepath"
)

// writeStateFile writes data to a file. On Windows, symlink attacks via
// os.WriteFile are less exploitable (no setuid/chown escalation model),
// so this is a straightforward wrapper.
func writeStateFile(stateDir, name string, data []byte, perm os.FileMode) error {
	path := filepath.Join(stateDir, name)
	return os.WriteFile(path, data, perm)
}

// chownStateFile is a no-op on Windows (no Unix ownership model).
func chownStateFile(path string, uid, gid int) error {
	return nil
}

// chownStateDir is a no-op on Windows.
func chownStateDir(path string, uid, gid int) error {
	return nil
}
