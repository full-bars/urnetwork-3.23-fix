package urnettools

import (
	"os"
	"os/exec"
	"strconv"
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

// containerLogsFollow tails a container's docker logs: the last n lines, then
// follow (docker logs --tail N -f).
func containerLogsFollow(name string, n int) error {
	cmd := exec.Command(dockerCLI(), "logs", "--tail", strconv.Itoa(n), "-f", name)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// containerFollowFile tails a file inside a container via docker exec: the
// last n lines, then follow. Used for the RAMLOG (/dev/shm) when the
// container runs with URNETWORK_RAMLOGS.
func containerFollowFile(name, path string, n int) error {
	cmd := exec.Command(dockerCLI(), "exec", name, "tail", "-n", strconv.Itoa(n), "-f", path)
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

// containerIDByName resolves a container name to its ID via docker ps (used
// where the API needs the ID; most commands accept the name directly).
func containerIDByName(name string) string {
	cmd := exec.Command(dockerCLI(), "ps", "-aqf", "name="+name)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	// -aqf can match several containers (name prefix); take the first ID.
	for _, line := range strings.Split(string(out), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			return id
		}
	}
	return ""
}
