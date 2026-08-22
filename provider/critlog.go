package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const critLogMaxSize = 1 * 1024 * 1024

func critLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "events.log"), nil
}

func critLog(format string, args ...any) {
	p, err := critLogPath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return
	}

	ts := time.Now().UTC().Format("2006-01-02T15:04:05")
	msg := fmt.Sprintf("%s "+format+"\n", append([]any{ts}, args...)...)

	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		f.Write([]byte(msg))
		return
	}
	if info.Size() > critLogMaxSize {
		f.Close()
		rotateCritLog(p)
		f, err = os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return
		}
		defer f.Close()
	}

	f.Write([]byte(msg))
	f.Sync()
}

func rotateCritLog(path string) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		os.Remove(path)
		return
	}

	half := len(b) / 2
	start := 0
	for i := half; i < len(b); i++ {
		if b[i] == '\n' {
			start = i + 1
			break
		}
	}
	if start == 0 {
		start = half
		for start > 0 && b[start-1] != '\n' {
			start--
		}
	}

	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(b[start:])
	f.Sync()
}

func critLogPanic(r any, stack []byte) {
	critLog("PANIC: %v\n%s", r, string(stack))

	buf := make([]byte, 64*1024)
	n := runtime.Stack(buf, true)
	critLog("GOROUTINE DUMP:\n%s", string(buf[:n]))
}
