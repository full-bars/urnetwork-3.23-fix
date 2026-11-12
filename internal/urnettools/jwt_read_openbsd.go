package urnettools

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const maxJWTBytes = 1024 * 1024

func readJWTFile(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("JWT is not a regular file: %s", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxJWTBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxJWTBytes {
		return nil, fmt.Errorf("JWT exceeds %d bytes", maxJWTBytes)
	}
	return raw, nil
}
