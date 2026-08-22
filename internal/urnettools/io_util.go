package urnettools

import (
	"io"
	"os"
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
