package urnettools

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// diagClientTimeout is how long `diag` waits for the provider. The capture is a
// 30 second CPU window plus the other profiles; the provider itself gives up at
// 80 seconds, so this leaves it room to answer with its own error first.
var diagClientTimeout = 95 * time.Second

// diagReply mirrors the provider's diagResult, carried in the control response's
// Value field as JSON.
type diagReply struct {
	Dir           string   `json:"dir"`
	Files         []string `json:"files"`
	Notes         []string `json:"notes"`
	RSSBytes      uint64   `json:"rss_bytes"`
	HeapInuse     uint64   `json:"heap_inuse_bytes"`
	HeapSys       uint64   `json:"heap_sys_bytes"`
	Goroutines    int      `json:"goroutines"`
	UptimeSeconds float64  `json:"uptime_seconds"`
	Clients       int64    `json:"clients"`
}

// cmdDiag implements `urnet-tools diag`: ask the running provider to write a
// profile bundle (heap, goroutines, 30 s of CPU, and a plain summary) into its
// state directory, and print where it is. It takes no flags or arguments; every
// parameter is fixed on the provider side. It never restarts anything and opens
// no network port: the request goes over the owner-only control socket.
func cmdDiag(args []string) error {
	return runDiagCmd(args, discoverStatusFn(), os.Stdout, os.Stderr)
}

func runDiagCmd(args []string, providers []Provider, out, progress io.Writer) error {
	if len(args) > 0 {
		return fmt.Errorf("diag takes no flags or arguments (got %q); run it as just `urnet-tools diag`", args[0])
	}
	p, _, err := selectTargetOrSoleAccessible(providers, Target{}, false)
	if err != nil {
		if len(providers) > 1 {
			return fmt.Errorf("%w\n(diag takes no flags: run it as the user the provider you mean runs as, without sudo, and it targets that one)", err)
		}
		return err
	}
	label := providerLabel(p)
	if !p.Running {
		return fmt.Errorf("provider %s is not running, so there is nothing to capture. Start it first with `urnet-tools start`, wait for the memory or CPU to grow again, then run `urnet-tools diag`. A restarted provider has already lost the state diag exists to record", label)
	}
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state directory, so its control socket cannot be found. Run `urnet-tools status` to see what was discovered", label)
	}
	sock := filepath.Join(p.StateDir, "provider.sock")

	// Fail before announcing a capture that cannot start.
	if _, err := os.Stat(sock); err != nil {
		return diagTransportError(err, sock, p.StateDir, label)
	}

	fmt.Fprintf(progress, "Capturing diagnostics from %s: goroutines, heap, then a 30 second CPU profile. The provider keeps serving and is not restarted; this takes about 40 seconds.\n", label)
	resp, err := sendSocketRequestTimeout(sock, controlRequest{Cmd: "diag"}, diagClientTimeout)
	if err != nil {
		return diagTransportError(err, sock, p.StateDir, label)
	}
	if !resp.OK {
		if strings.HasPrefix(resp.Error, "unknown command") {
			return fmt.Errorf("provider %s is too old to know `diag`. Update it with `urnet-tools update` (then `urnet-tools hotswap` to switch without a gap), and run diag again", label)
		}
		return errors.New(strings.TrimSpace(strings.TrimPrefix(resp.Error, "diag: ")))
	}
	var r diagReply
	if err := json.Unmarshal([]byte(resp.Value), &r); err != nil || r.Dir == "" {
		return fmt.Errorf("provider %s answered diag with a reply this tool cannot read (%v); the tool and the provider are different versions, update both with `urnet-tools update`", label, err)
	}

	fmt.Fprintf(out, "Diagnostic bundle: %s\n", r.Dir)
	fmt.Fprintf(out, "  memory (RSS) %s   heap in use %s (after GC)   heap reserved %s\n",
		formatBytes(float64(r.RSSBytes)), formatBytes(float64(r.HeapInuse)), formatBytes(float64(r.HeapSys)))
	fmt.Fprintf(out, "  goroutines %d   clients %d   uptime %s\n",
		r.Goroutines, r.Clients, (time.Duration(r.UptimeSeconds * float64(time.Second))).Round(time.Second))
	fmt.Fprintf(out, "  files: %s\n", strings.Join(r.Files, ", "))
	for _, n := range r.Notes {
		fmt.Fprintf(out, "  note: %s\n", n)
	}
	fmt.Fprintf(out, "Read summary.txt first. The bundle can contain session tokens, so do not post it publicly.\n")
	return nil
}

// diagTransportError turns a socket failure into a message that says what to do.
func diagTransportError(err error, sock, stateDir, label string) error {
	var ne net.Error
	switch {
	case errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM):
		return fmt.Errorf("not allowed to open the control socket %s (%v). It is owner-only: run `urnet-tools diag` as the user provider %s runs as, or with sudo", sock, err, label)
	case errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("the control socket %s is not available (%v), so the provider cannot be asked. It may still be starting, or it may predate the control socket. Check `urnet-tools status`, wait a minute if it just started, and run diag again", sock, err)
	case errors.As(err, &ne) && ne.Timeout(), errors.Is(err, os.ErrDeadlineExceeded):
		return fmt.Errorf("provider %s did not answer within %s. A capture may still be finishing: look in %s for a new bundle, and try again in a minute. If it keeps happening the provider is overloaded, which is itself worth noting", label, diagClientTimeout, filepath.Join(stateDir, "diag"))
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return fmt.Errorf("the connection to provider %s closed before it answered. If a hot swap or restart was in progress the capture was abandoned and left no files; run diag again once `urnet-tools status` shows it running", label)
	}
	return fmt.Errorf("could not ask provider %s for a capture over %s: %v", label, sock, err)
}
