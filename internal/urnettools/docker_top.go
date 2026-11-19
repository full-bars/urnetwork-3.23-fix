package urnettools

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/term"
)

// consumeDockerTopTarget ignores the value of --interval when looking for a
// container name, including when that value happens to be a container name.
func consumeDockerTopTarget(providers []Provider, t Target, args []string) (Target, []string) {
	if hasExplicitTarget(t) {
		return t, args
	}
	for i := 0; i < len(args); i++ {
		if args[i] == "--interval" {
			i++
			continue
		}
		if strings.HasPrefix(args[i], "-") {
			continue
		}
		for _, p := range providers {
			if p.Unit == args[i] {
				t.Unit = args[i]
				out := append([]string{}, args[:i]...)
				return t, append(out, args[i+1:]...)
			}
		}
	}
	return t, args
}

// dockerTopArgs keeps terminal environment forwarding specific to top.
func dockerTopArgs(name string, args []string, lookup func(string) (string, bool), persistent bool) []string {
	out := []string{"exec", "-it"}
	for _, key := range []string{"COLORTERM", "TERM"} {
		if _, ok := lookup(key); ok {
			out = append(out, "-e", key)
		}
	}
	if persistent {
		out = append(out, "-e", "XDG_CONFIG_HOME=/root/.urnetwork/.config")
	}
	out = append(out, name, "/usr/local/bin/urnet-tools-go", "top")
	return append(out, args...)
}

func containerTopExec(name string, args []string) error {
	return containerTopExecWithTerminal(name, args, term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())))
}

func containerTopExecWithTerminal(name string, args []string, terminal bool) error {
	if !terminal {
		return errors.New("top needs an interactive terminal on stdin and stdout; use urnet-docker status for scripts")
	}
	check := exec.Command(dockerCLI(), "exec", name, "sh", "-c", "if [ -x /usr/local/bin/urnet-tools-go ]; then exit 0; else exit 42; fi")
	if err := check.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 42 {
			return fmt.Errorf("this container's image has no urnet-tools top; update the image")
		}
		return fmt.Errorf("cannot check top in container %q (it may have stopped): %w", name, err)
	}
	// Only place settings on the state mount when Docker confirms it is writable.
	inspect := exec.Command(dockerCLI(), "inspect", "-f", `{{range .Mounts}}{{if and (eq .Destination "/root/.urnetwork") .RW}}yes{{end}}{{end}}`, name)
	out, err := inspect.Output()
	persistent := err == nil && strings.TrimSpace(string(out)) == "yes"
	if !persistent {
		fmt.Fprintln(os.Stderr, "top settings are stored inside this container and may be lost when it is recreated")
	}
	cmd := exec.Command(dockerCLI(), dockerTopArgs(name, args, os.LookupEnv, persistent)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && (exit.ExitCode() == 126 || exit.ExitCode() == 127) {
			return fmt.Errorf("this container's image has no usable urnet-tools top; update the image: %w", err)
		}
		return fmt.Errorf("docker top failed for container %q (it may have stopped): %w", name, err)
	}
	return nil
}
