package urnettools

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// A later-sorting drop-in can reset RestartForceExitStatus with an empty
// assignment; the effective-state check after daemon-reload catches this.
const sigpipeDropInName = "90-urnet-tools-restart.conf"
const sigpipeDropInContent = "[Service]\nRestartForceExitStatus=SIGPIPE\n"

func dropInDirForUnit(p Provider) (string, error) {
	unitPath, err := resolveUnitFilePath(p)
	if err != nil {
		return "", fmt.Errorf("resolve unit path: %w", err)
	}
	if unitPath == "" {
		return "", fmt.Errorf("resolve unit path: empty path")
	}
	return filepath.Join(filepath.Dir(unitPath), p.Unit+".d"), nil
}

func ensureSigpipeRestartDropIn(p Provider) (bool, error) {
	if runtime.GOOS != "linux" || p.Unit == "" || p.Supervisor == "openrc" {
		return false, nil
	}
	return reconcileSigpipeRestart(p, runSigpipeSystemctl, dropInDirForUnit, daemonReloadForUnit)
}

// reconcileSigpipeRestart never restarts the provider. A warning is returned as
// an error so the caller can report it and continue the update.
func reconcileSigpipeRestart(p Provider, run sigpipeRunner, dirFor func(Provider) (string, error), reload func(Provider) error) (bool, error) {
	state, err := showSigpipeState(p, run)
	if err != nil {
		return false, fmt.Errorf("could not determine effective SIGPIPE restart state: %w", err)
	}
	if sigpipeContains(state.prevent) {
		return false, fmt.Errorf("operator setting RestartPreventExitStatus contains SIGPIPE; cannot install effective restart protection (%s)", state)
	}
	if !state.risk() {
		return false, nil
	}
	dir, err := dirFor(p)
	if err != nil {
		return false, fmt.Errorf("drop-in dir: %w", err)
	}
	path := filepath.Join(dir, sigpipeDropInName)
	existing, err := readUnitFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read drop-in: %w", err)
	}
	if string(existing) == sigpipeDropInContent {
		return false, fmt.Errorf("SIGPIPE restart drop-in is present but ineffective (%s); check later overrides", state)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir drop-in dir: %w", err)
	}
	if err := replaceUnitFileIn(p.StateHome, path, []byte(sigpipeDropInContent)); err != nil {
		return false, fmt.Errorf("write drop-in: %w", err)
	}
	if err := reload(p); err != nil {
		return true, fmt.Errorf("daemon-reload: %w", err)
	}
	state, err = showSigpipeState(p, run)
	if err != nil {
		return true, fmt.Errorf("cannot verify effective SIGPIPE restart state after daemon-reload: %w", err)
	}
	if state.risk() {
		return true, fmt.Errorf("SIGPIPE restart drop-in is ineffective after daemon-reload (%s); check later overrides", state)
	}
	fmt.Printf("installed SIGPIPE restart drop-in for %s at %s\n", p.Unit, path)
	return true, nil
}
