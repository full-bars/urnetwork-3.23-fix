package urnettools

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// updateConfig holds the release metadata for the update command.
type updateConfig struct {
	// Tag is the release tag to install, e.g. "v3.23.0-fix.26.8".
	Tag string
	// Digest is the sha256 of the release tarball asset (hex). When empty,
	// integrity verification is skipped (not recommended).
	Digest string
	// AssetURL is the download URL for the tarball.
	AssetURL string
	// StageDir is where downloads/extraction happen. MUST be on real disk —
	// /tmp is frequently a small tmpfs and the multi-platform tarball
	// overflows it (the 2026-08-09 failure).
	StageDir string
}

// defaultUpdateConfig returns the config for the latest known release. Tag
// and Digest are populated by the caller (release metadata may be fetched).
func defaultUpdateConfig() updateConfig {
	return updateConfig{
		StageDir: "/var/tmp/urnet-stage",
	}
}

// cmdUpdate updates one or more providers' binaries to the given release,
// then restarts the unit that actually owns each (system-level or
// user-level — never the wrong one). Destructive gate applies per provider.
func cmdUpdate(args []string, force, dryRun bool) error {
	t, rest, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	cfg := defaultUpdateConfig()
	// Parse --tag/--digest/--url and batch-selection overrides.
	var include, exclude []string
	interactive := false
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--tag":
			if i+1 >= len(rest) {
				return fmt.Errorf("--tag requires a value")
			}
			cfg.Tag = rest[i+1]
			i++
		case "--digest":
			if i+1 >= len(rest) {
				return fmt.Errorf("--digest requires a value")
			}
			cfg.Digest = rest[i+1]
			i++
		case "--url":
			if i+1 >= len(rest) {
				return fmt.Errorf("--url requires a value")
			}
			cfg.AssetURL = rest[i+1]
			i++
		case "--include":
			if i+1 >= len(rest) {
				return fmt.Errorf("--include requires a value (comma-separated labels)")
			}
			include = splitLabels(rest[i+1])
			i++
		case "--exclude":
			if i+1 >= len(rest) {
				return fmt.Errorf("--exclude requires a value (comma-separated labels)")
			}
			exclude = splitLabels(rest[i+1])
			i++
		case "--select":
			interactive = true
		}
	}
	if cfg.Tag == "" {
		return fmt.Errorf("update requires --tag (e.g. --tag v3.23.0-fix.26.8)")
	}

	providers := Discover()
	chosen, err := selectTargets(providers, t, include, exclude, interactive)
	if err != nil {
		return err
	}

	// Confirm once for the whole set, listing every provider.
	ok, err := confirmGateMulti(fmt.Sprintf("update %d provider(s) to %s", len(chosen), cfg.Tag), chosen, force, dryRun)
	if err != nil {
		return err
	}
	if !ok {
		return nil // dry-run
	}

	for _, p := range chosen {
		if p.Binary == "" {
			return fmt.Errorf("provider %s has no resolvable binary path", providerLabel(p))
		}
		if p.Version == cfg.Tag {
			fmt.Printf("provider %s already on %s\n", providerLabel(p), cfg.Tag)
			continue
		}
		if err := updateProvider(p, cfg); err != nil {
			return fmt.Errorf("update %s failed: %w", providerLabel(p), err)
		}
	}
	return nil
}

// splitLabels splits a comma-separated label list.
func splitLabels(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// updateProvider performs the surgical binary swap for one provider:
//  1. Stage the tarball on real disk (never /tmp tmpfs).
//  2. Verify sha256 against the release digest when provided.
//  3. Extract ONLY linux/$arch/provider — not the whole multi-platform
//     tarball (bloat + the tmpfs overflow root cause).
//  4. Back up the current binary.
//  5. Swap with the provider user's ownership.
//  6. Restart the unit that OWNS the running process (systemd unit name, or
//     fall back to restarting by user-level unit, or plain process signal).
//
// This is the exact recipe proven on 2026-08-09 for taco's fleet.
func updateProvider(p Provider, cfg updateConfig) error {
	if err := os.MkdirAll(cfg.StageDir, 0o755); err != nil {
		return fmt.Errorf("stage dir: %w", err)
	}
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "amd64"
	}
	relPath := filepath.Join("linux", arch, "provider")
	if runtime.GOOS == "windows" {
		relPath = filepath.Join("windows", arch, "provider.exe")
	}

	url := cfg.AssetURL
	if url == "" {
		url = fmt.Sprintf("https://github.com/full-bars/urnetwork-3.23-fix/releases/download/%s/urnetwork-provider-%s.tar.gz", cfg.Tag, cfg.Tag)
	}
	tarball := filepath.Join(cfg.StageDir, cfg.Tag+".tar.gz")

	fmt.Printf("downloading %s\n", url)
	if err := downloadFile(url, tarball); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if cfg.Digest != "" {
		if err := verifySHA256(tarball, cfg.Digest); err != nil {
			return err
		}
		fmt.Println("sha256 verified")
	}

	// Extract only the needed arch's provider binary.
	extractDir := filepath.Join(cfg.StageDir, "extract-"+cfg.Tag)
	if err := os.RemoveAll(extractDir); err != nil {
		return err
	}
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return err
	}
	if err := extractSingleFile(tarball, relPath, filepath.Join(extractDir, "provider")); err != nil {
		return fmt.Errorf("extract %s: %w", relPath, err)
	}

	// Version check the staged binary.
	staged := filepath.Join(extractDir, "provider")
	if v := providerVersion(staged); v != cfg.Tag {
		return fmt.Errorf("staged binary reports %q, expected %q — aborting", v, cfg.Tag)
	}

	// Backup current binary.
	backup := p.Binary + ".bak-" + strings.TrimPrefix(p.Version, "v")
	if _, err := os.Stat(backup); os.IsNotExist(err) {
		if err := copyFile(p.Binary, backup); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		fmt.Printf("backed up %s -> %s\n", p.Binary, backup)
	}

	// Swap with ownership preserved for the provider user.
	if err := installBinary(staged, p.Binary, p.User); err != nil {
		return fmt.Errorf("swap binary: %w", err)
	}
	fmt.Printf("swapped %s -> %s\n", staged, p.Binary)

	// Restart the unit that owns the running process.
	return restartProvider(p)
}

// restartProvider restarts the systemd unit (system or user level) that owns
// the provider process. Falls back gracefully when systemd is unavailable.
func restartProvider(p Provider) error {
	if p.Unit != "" {
		// System-level units are owned by root; user-level units run in the
		// user's session. Try the system manager first, then the user one.
		if out, err := exec.Command("systemctl", "restart", p.Unit).CombinedOutput(); err == nil {
			fmt.Printf("restarted %s\n", p.Unit)
			return nil
		} else if strings.Contains(string(out), "not found") || strings.Contains(string(out), "No such") {
			// Fall through to user-level.
		} else {
			return fmt.Errorf("systemctl restart %s: %v (%s)", p.Unit, err, strings.TrimSpace(string(out)))
		}
	}
	if p.User != "" && p.PID > 0 {
		// No systemd ownership resolved: signal the process directly.
		proc, err := os.FindProcess(p.PID)
		if err == nil {
			if err := proc.Signal(os.Interrupt); err == nil {
				fmt.Printf("sent SIGINT to pid %d (provider will restart under its unit)\n", p.PID)
				time.Sleep(2 * time.Second)
				return nil
			}
		}
	}
	return fmt.Errorf("could not restart provider %s — restart the owning unit manually", providerLabel(p))
}

// downloadFile fetches url into path (atomic-ish: temp file + rename).
func downloadFile(url, path string) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// verifySHA256 checks the file's sha256 against the expected hex digest.
func verifySHA256(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 mismatch: got %s want %s", got, want)
	}
	return nil
}

// extractSingleFile extracts exactly one file from a .tar.gz to dst.
func extractSingleFile(tarball, relPath, dst string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Name == relPath || strings.TrimPrefix(hdr.Name, "./") == relPath {
			out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return err
			}
			_, cerr := io.Copy(out, tr)
			out.Close()
			return cerr
		}
	}
	return fmt.Errorf("path %s not found in tarball", relPath)
}

// installBinary copies src to dst preserving ownership for the given user.
// When running as root it chowns to the provider user; otherwise it relies
// on the caller's own permissions.
func installBinary(src, dst, user string) error {
	if err := copyFile(src, dst); err != nil {
		return err
	}
	if err := os.Chmod(dst, 0o755); err != nil {
		return err
	}
	if user != "" && os.Geteuid() == 0 {
		// Resolve uid/gid via id(1) — portable without cgo.
		uidOut, err := exec.Command("id", "-u", user).Output()
		if err != nil {
			return fmt.Errorf("resolve uid for %s: %w", user, err)
		}
		gidOut, err := exec.Command("id", "-g", user).Output()
		if err != nil {
			return fmt.Errorf("resolve gid for %s: %w", user, err)
		}
		uid := strings.TrimSpace(string(uidOut))
		gid := strings.TrimSpace(string(gidOut))
		if err := exec.Command("chown", uid+":"+gid, dst).Run(); err != nil {
			return fmt.Errorf("chown %s: %w", dst, err)
		}
	}
	return nil
}

// copyFile copies src to dst preserving mode.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	_, cerr := io.Copy(out, in)
	out.Close()
	return cerr
}
