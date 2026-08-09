package urnettools

import (
	"io"
	"os"
	"path/filepath"
)

// writeTempJWT writes raw JWT content to a temp file and returns its path.
// Used to hand container-read JWT bytes to decodeJWT (which reads from
// disk). Caller should remove the file when done.
func writeTempJWT(raw string) (string, error) {
	f, err := os.CreateTemp("", "urnet-jwt-*.txt")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(raw); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// stdoutWriter returns the process stdout as an io.Writer.
func stdoutWriter() io.Writer { return os.Stdout }

// stderrWriter returns the process stderr as an io.Writer.
func stderrWriter() io.Writer { return os.Stderr }

// removeQuietly removes a path, ignoring errors (best-effort cleanup).
func removeQuietly(path string) {
	_ = os.Remove(path)
}

// homeDir returns the current user's home directory (used in fallbacks).
func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

// joinStateDir joins a home path with the .urnetwork state dir.
func joinStateDir(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".urnetwork")
}
