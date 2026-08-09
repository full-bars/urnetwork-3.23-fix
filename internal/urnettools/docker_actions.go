package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// containerExecByName runs a command inside a container by name/id.
func containerExecByName(name string, args ...string) error {
	full := append([]string{"exec", name}, args...)
	cmd := exec.Command(dockerCLI(), full...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// containerRestartByName restarts a container by name/id.
func containerRestartByName(name string) error {
	cmd := exec.Command(dockerCLI(), "restart", name)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// containerLogs tails docker logs for a container.
func containerLogs(name, n string) error {
	cmd := exec.Command(dockerCLI(), "logs", "--tail", n, name)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// containerReadFileSafe reads a file from a container, tolerating errors
// (returns "", err when the read fails — caller decides fallback).
func containerReadFileSafe(name, path string) (string, error) {
	cmd := exec.Command(dockerCLI(), "exec", name, "cat", path)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// tailLines returns the last n lines of s.
func tailLines(s, n string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	count := 0
	fmt.Sscanf(n, "%d", &count)
	if count <= 0 || count > len(lines) {
		count = len(lines)
	}
	if count > len(lines) {
		count = len(lines)
	}
	return strings.Join(lines[len(lines)-count:], "\n") + "\n"
}

// containerIDByName resolves a container name to its ID via docker ps (used
// where the API needs the ID; most commands accept the name directly).
func containerIDByName(name string) string {
	cmd := exec.Command(dockerCLI(), "ps", "-aqf", "name="+name)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var _ = fmt.Sprintf // keep fmt import if unused in future edits
