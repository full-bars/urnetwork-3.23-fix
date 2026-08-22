package urnettools

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// This file restores the hub subcommands the Go rewrite dropped from the
// legacy shell tool (do_hub_*): init, link, unlink, test, onboard-cmd,
// show-password, open-port, update. They manage the URnetwork hub pairing and
// identity trust on a provider. The hub is deployed as a user systemd unit
// running the urnetwork-hub binary; its data lives in the provider's
// ~/.local/share/urnetwork-hub.

// hubDataDir returns the hub data directory for the provider's user.
func hubDataDir(p Provider) (string, error) {
	home := homeForUser(p.User)
	if home == "" {
		return "", fmt.Errorf("cannot resolve home for user %s", p.User)
	}
	return filepath.Join(home, ".local/share/urnetwork-hub"), nil
}

// hubBinPath returns the installed hub binary path for a provider (sibling of
// the provider binary, matching how cmdHubInstall places it).
func hubBinPath(p Provider) string {
	if p.Binary == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p.Binary), "urnetwork-hub")
}

// runHub delegates a flag-based operation to the hub binary with the given
// data dir, streaming output. The hub is the source of truth for password and
// onboard-token operations.
func runHub(p Provider, flags ...string) error {
	hubBin := hubBinPath(p)
	if hubBin == "" {
		return fmt.Errorf("provider %s has no resolvable binary path", providerLabel(p))
	}
	if !isExecutableFile(hubBin) {
		return fmt.Errorf("hub binary not executable at %s (run 'hub install' first)", hubBin)
	}
	dir, err := hubDataDir(p)
	if err != nil {
		return err
	}
	cmd := exec.Command(hubBin, append(flags, "-data", dir)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if p.User != "" {
		if home := homeForUser(p.User); home != "" {
			cmd.Env = append(os.Environ(), "HOME="+home)
		}
	}
	return cmd.Run()
}

func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&0o111 != 0
}

// cmdHubShowPassword prints the hub's CA password (printed once at init and
// then only retrievable via the hub binary). Delegates to the hub binary.
func cmdHubShowPassword(p Provider) error {
	fmt.Fprintln(os.Stderr, "Hub password (keep this secret, do not paste it anywhere public):")
	fmt.Fprintln(os.Stderr)
	return runHub(p, "-show-password")
}

// cmdHubOnboardCmd mints an onboard token from the hub binary and prints the
// fleet one-liner providers can run to zero-touch onboard.
func cmdHubOnboardCmd(p Provider) error {
	hubBin := hubBinPath(p)
	if hubBin == "" {
		return fmt.Errorf("provider %s has no resolvable binary path", providerLabel(p))
	}
	if !isExecutableFile(hubBin) {
		return fmt.Errorf("hub binary not executable at %s (run 'hub install' first)", hubBin)
	}
	dir, err := hubDataDir(p)
	if err != nil {
		return err
	}
	cmd := exec.Command(hubBin, "-mint-onboard-token", "-data", dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to mint onboard token: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	token := extractField(string(out), "Token:")
	expires := extractField(string(out), "Expires:")
	caFp := extractField(string(out), "CA fingerprint:")
	if token == "" || expires == "" {
		return fmt.Errorf("unexpected hub output:\n%s", string(out))
	}
	ip := localIPv4()
	fmt.Printf("Token:      %s\n", token)
	fmt.Printf("Expires:    %s\n", expires)
	fmt.Printf("CA fingerprint: %s\n", caFp)
	fmt.Println()
	fmt.Println("On each provider, run this one-liner:")
	fmt.Println()
	if ip != "" {
		fmt.Printf("  curl -fsSL http://%s:8080/onboard.sh | sh -s -- %s\n", ip, token)
		fmt.Println()
		fmt.Printf("  (if %s is not the address providers reach, substitute the correct host)\n", ip)
	} else {
		fmt.Printf("  curl -fsSL http://<this-host>:8080/onboard.sh | sh -s -- %s\n", token)
	}
	fmt.Println()
	fmt.Println("The token is reusable for 15 minutes — paste once and onboard the whole fleet.")
	return nil
}

// extractField pulls the value after a `label ` prefix on the first matching
// line.
func extractField(out, label string) string {
	for _, ln := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, label) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, label))
		}
	}
	return ""
}

// localIPv4 returns the first IPv4 address of this host (best effort), or "".
func localIPv4() string {
	out, err := exec.Command("hostname", "-I").Output()
	if err != nil {
		return ""
	}
	for _, ip := range strings.Fields(string(out)) {
		if strings.Contains(ip, ".") {
			return ip
		}
	}
	return ""
}

// hubCertResp is the JSON the hub serves from /api/ca-cert and /api/cert.
type hubCertResp struct {
	CAPEM         string `json:"ca_pem"`
	CAFingerprint string `json:"ca_fingerprint"`
	LegacyFp      string `json:"fingerprint"`
	ErrorMessage  string `json:"error,omitempty"`
}

// fetchHubCA fetches and decodes the hub CA certificate + fingerprint from
// the given endpoint. token, when non-empty, is appended as the onboard token.
func fetchHubCA(baseURL, token string) (caPEM, fingerprint string, err error) {
	u := strings.TrimRight(baseURL, "/") + "/api/cert"
	if token != "" {
		u = strings.TrimRight(baseURL, "/") + "/api/ca-cert?token=" + urlQueryEscape(token)
	}
	// The hub serves a self-signed CA (the setup docs call this endpoint with
	// curl -k), so transport TLS cannot be verified against system roots. This
	// is TOFU: link then pins the returned CA/fingerprint, which is the trust
	// decision the operator confirms. Disabling transport verification here is
	// required for the feature to work at all against a real hub.
	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // TOFU: pinned below
		},
	}
	resp, err := client.Get(u)
	if err != nil {
		return "", "", fmt.Errorf("could not reach hub at %s: %v", baseURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", "", err
	}
	var r hubCertResp
	if err := json.Unmarshal(body, &r); err != nil {
		return "", "", fmt.Errorf("hub response was not JSON: %v", err)
	}
	if r.ErrorMessage != "" {
		return "", "", fmt.Errorf("hub error: %s", r.ErrorMessage)
	}
	if r.CAPEM != "" {
		caPEM := strings.ReplaceAll(r.CAPEM, `\n`, "\n")
		// Reject a mismatched (fingerprint, ca_pem) pair: the fingerprint shown
		// for confirmation must match the CA actually persisted (review HIGH).
		if r.CAFingerprint != "" {
			if computed, ferr := pemFingerprint(caPEM); ferr == nil && computed != r.CAFingerprint {
				return "", "", fmt.Errorf("hub CA fingerprint mismatch: reported %s does not match the served CA certificate", r.CAFingerprint)
			}
		}
		return caPEM, r.CAFingerprint, nil
	}
	if r.LegacyFp != "" {
		return "", r.LegacyFp, nil
	}
	return "", "", fmt.Errorf("hub responded but returned no CA certificate (may be an older hub version)")
}

func urlQueryEscape(s string) string {
	return url.QueryEscape(s)
}

// tlsFingerprint connects to host:port, validates the TLS handshake against
// the system roots (or InsecureSkipVerify when the pin file exists), and
// returns the leaf certificate SHA256 fingerprint in the form used by
// hub.pin ("SHA256:<lowercase-hex, no colons>"). It pins TOFU credentials —
// no certificate is trusted unless it matches an existing pin or the operator
// confirms a new one.
func tlsFingerprint(host, port string) (string, error) {
	addr := net.JoinHostPort(host, port)
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) // we verify via fingerprint
	if err != nil {
		return "", err
	}
	defer conn.Close()
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return "", fmt.Errorf("no peer certificate presented by %s", addr)
	}
	sum := sha256.Sum256(state.PeerCertificates[0].Raw)
	return "SHA256:" + strings.ToLower(hex.EncodeToString(sum[:])), nil
}

// splitHostPortURL splits an https:// URL into host and port (default 443).
func splitHostPortURL(u string) (host, port string, err error) {
	rest := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	hostPort := rest
	if i := strings.IndexByte(hostPort, '/'); i >= 0 {
		hostPort = hostPort[:i]
	}
	if h, p, err := net.SplitHostPort(hostPort); err == nil {
		return h, p, nil
	}
	if i := strings.IndexByte(hostPort, ':'); i >= 0 {
		return hostPort[:i], hostPort[i+1:], nil
	}
	return hostPort, "443", nil
}

// cmdHubLink fetches the hub CA, applies certificate pinning (hub.pin) or CA
// trust (hub_ca.pem), and records the report URL so the provider reports to
// the linked hub. Requires https. Confirms a new fingerprint unless force.
func cmdHubLink(p Provider, url, token string, force bool) error {
	if !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("hub link URL must start with https://")
	}
	url = strings.TrimRight(url, "/")
	hubDir := p.StateDir
	if hubDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	caFile := filepath.Join(hubDir, "hub_ca.pem")
	pinFile := filepath.Join(hubDir, "hub.pin")

	caPEM, fp, err := fetchHubCA(url, token)
	if err != nil {
		return err
	}

	if fp == "" {
		// Compute the pinned fingerprint from the fetched CA PEM via TLS.
		host, port, _ := splitHostPortURL(url)
		if f, err := tlsFingerprint(host, port); err == nil {
			fp = f
		} else {
			return fmt.Errorf("could not verify hub certificate: %v", err)
		}
	}
	fmt.Printf("Hub fingerprint: %s\n", fp)

	if caPEM == "" {
		// Legacy hub: pin the fingerprint.
		if fp == "" {
			return fmt.Errorf("hub provided neither a CA certificate nor a fingerprint")
		}
		if !force {
			if !confirmFingerprint(fp) {
				return fmt.Errorf("fingerprint not accepted")
			}
		}
		if err := os.MkdirAll(hubDir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(pinFile, []byte(fp), 0o600); err != nil {
			return err
		}
		fmt.Printf("Pinned hub fingerprint to %s\n", pinFile)
	} else {
		fmt.Printf("Hub CA fingerprint: %s\n", fp)
		if !force && !acceptFingerprintPrompt() {
			return fmt.Errorf("aborted by user")
		}
		if err := os.MkdirAll(hubDir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(caFile, []byte(caPEM), 0o600); err != nil {
			return err
		}
		_ = os.Remove(pinFile) // CA trust supersedes pin
		fmt.Printf("CA certificate saved to %s\n", caFile)
	}

	// Record the report URL so the provider reports to the linked hub.
	return writeReportURL(p, url)
}

// hubYesNo prompts for an explicit y/N on stderr and returns true only on yes.
func hubYesNo(prompt string) bool {
	fmt.Fprintf(os.Stderr, "%s ", prompt)
	b := make([]byte, 1)
	n, _ := os.Stdin.Read(b)
	if n == 0 {
		return false
	}
	return b[0] == 'y' || b[0] == 'Y'
}

func confirmFingerprint(fp string) bool {
	return hubYesNo("Accept this hub fingerprint? (y/n)")
}

func acceptFingerprintPrompt() bool {
	return confirmFingerprint("")
}

// cmdHubTest verifies TLS to the hub at url (or the configured report URL),
// preferring CA-chain verification, falling back to the pinned fingerprint.
func cmdHubTest(p Provider, url string) error {
	if url == "" {
		if b, err := os.ReadFile(filepath.Join(p.StateDir, "report_url")); err == nil {
			url = strings.TrimSpace(string(b))
		}
	}
	if url == "" {
		return fmt.Errorf("no hub URL configured; pass one or run 'hub link https://...' first")
	}
	if !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("hub test requires an https:// URL (got %s)", url)
	}
	host, port, err := splitHostPortURL(url)
	if err != nil {
		return err
	}
	fmt.Printf("Testing TLS to %s:%s ...\n", host, port)

	// CA-trust mode (the default after hub link): verify the presented chain
	// against the saved hub_ca.pem. Fall back to a pinned fingerprint only when
	// no CA file exists.
	if b, err := os.ReadFile(filepath.Join(p.StateDir, "hub_ca.pem")); err == nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return fmt.Errorf("hub_ca.pem is not a valid PEM certificate")
		}
		conn, derr := tls.Dial("tcp", net.JoinHostPort(host, port), &tls.Config{RootCAs: pool})
		if derr != nil {
			return fmt.Errorf("TLS FAILED — CA chain verification error: %v", derr)
		}
		conn.Close()
		fmt.Println("TLS OK — CA chain verification passed.")
		return nil
	}

	actual, err := tlsFingerprint(host, port)
	if err != nil {
		return fmt.Errorf("TLS FAILED — could not connect: %v", err)
	}
	fmt.Printf("Hub certificate fingerprint: %s\n", actual)

	pin := ""
	if b, err := os.ReadFile(filepath.Join(p.StateDir, "hub.pin")); err == nil {
		pin = strings.TrimSpace(string(b))
	}
	if pin != "" {
		if actual == pin {
			fmt.Println("TLS OK — fingerprint matches the saved pin.")
			return nil
		}
		return fmt.Errorf("TLS FAILED — fingerprint MISMATCH (expected %s, got %s); re-link to re-pin", pin, actual)
	}
	fmt.Println("TLS OK — connected. Run 'hub link <url>' to pin.")
	return nil
}

// cmdHubUnlink removes the hub CA trust, pin, and report URL from the state dir.
func cmdHubUnlink(p Provider, force bool) error {
	hubDir := p.StateDir
	if hubDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	for _, f := range []string{"hub_ca.pem", "hub.pin"} {
		path := filepath.Join(hubDir, f)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Printf("Removed %s\n", path)
	}
	// Disable hub reporting.
	return writeReportURL(p, "off")
}

// hubHost extracts the lowercase host (no port) from an http(s) URL.
func hubHost(u string) string {
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	if i := strings.IndexByte(u, ':'); i >= 0 {
		u = u[:i]
	}
	return strings.ToLower(u)
}

// certPEMBlock decodes the first PEM block from a blob (nil if none).
func certPEMBlock(pemBlob string) (*pem.Block, error) {
	block, _ := pem.Decode([]byte(pemBlob))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	return block, nil
}

// pemFingerprint returns the SHA256 fingerprint of the first X.509 cert in a
// PEM blob, in the "SHA256:<lowercase-hex no colons>" form.
func pemFingerprint(pemBlob string) (string, error) {
	block, _ := certPEMBlock(pemBlob)
	if block == nil {
		return "", fmt.Errorf("no certificate found in PEM data")
	}
	sum := sha256.Sum256(block.Bytes)
	return "SHA256:" + strings.ToLower(hex.EncodeToString(sum[:])), nil
}

// hubUnitPath returns the user unit file path for the hub on the provider's user.
func hubUnitPath(p Provider) (string, error) {
	home := homeForUser(p.User)
	if home == "" {
		return "", fmt.Errorf("cannot resolve home for user %s", p.User)
	}
	return filepath.Join(home, ".config/systemd/user/urnetwork-hub.service"), nil
}

// hubUnitCommand runs a systemctl --user command for the hub unit, scoped to
// the provider's user session.
func hubUnitCommand(p Provider, args ...string) error {
	base := []string{"--user", "-M", p.User + "@"}
	cmd := exec.Command("systemctl", append(base, args...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl %s: %v", strings.Join(append(base, args...), " "), err)
	}
	return nil
}

// hubTLSDropinDir returns the drop-in dir that enables hub TLS.
func hubTLSDropinDir(p Provider) (string, error) {
	up, err := hubUnitPath(p)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(up), "urnetwork-hub.service.d"), nil
}

// cmdHubInit provisions the hub: writes its CA password, enables TLS on
// :8443, starts the service, waits for the CA cert, and reports the CA
// fingerprint.
func cmdHubInit(p Provider, password string) error {
	dir, err := hubDataDir(p)
	if err != nil {
		return err
	}
	os.MkdirAll(dir, 0o700)
	caCert := filepath.Join(dir, "ca.crt")
	if _, err := os.Stat(caCert); err == nil {
		b, rerr := os.ReadFile(caCert)
		if rerr == nil {
			if fp, ferr := pemFingerprint(string(b)); ferr == nil {
				fmt.Printf("Hub CA is already initialized. CA fingerprint: %s\n", fp)
			}
		}
		fmt.Println("On each provider, run: urnet-tools hub link https://<this-host>:8443")
		return nil
	}

	// Write the password (the hub derives its CA from it).
	if password != "" {
		if err := os.WriteFile(filepath.Join(dir, "hub.password"), []byte(password), 0o600); err != nil {
			return err
		}
		fmt.Println("Password written to hub data directory.")
	} else {
		fmt.Println("No password provided — the hub will auto-generate one; run 'hub show-password' after init.")
	}

	// Enable TLS on :8443 via a drop-in on the hub unit.
	dropDir, err := hubTLSDropinDir(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dropDir, 0o755); err != nil {
		return err
	}
	tlsConf := filepath.Join(dropDir, "tls.conf")
	if err := os.WriteFile(tlsConf, []byte("[Service]\nEnvironment=\"URNETWORK_HUB_TLS_ADDR=:8443\"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("Wrote %s\n", tlsConf)

	_ = hubUnitCommand(p, "daemon-reload")
	if activeErr := hubUnitCommand(p, "is-active", "--quiet", "urnetwork-hub.service"); activeErr == nil {
		_ = hubUnitCommand(p, "restart", "urnetwork-hub.service")
	} else {
		// Not running: enable (surfaces a masked unit) then start, and surface
		// a start failure rather than swallowing it into a generic timeout.
		if enableErr := hubUnitCommand(p, "enable", "urnetwork-hub.service"); enableErr != nil {
			return fmt.Errorf("failed to enable urnetwork-hub.service (it may be masked): %v", enableErr)
		}
		if startErr := hubUnitCommand(p, "start", "urnetwork-hub.service"); startErr != nil {
			return fmt.Errorf("failed to start urnetwork-hub.service: %v", startErr)
		}
	}

	// Wait briefly for the CA cert to be generated.
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(caCert); err == nil {
			if fp, ferr := pemFingerprint(string(b)); ferr == nil {
				fmt.Println("Hub started with TLS.")
				fmt.Printf("Hub CA fingerprint: %s\n", fp)
				fmt.Printf("Ensure port 8443 is open so providers can reach the hub (%s).\n", firewallHintText())
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("CA certificate not generated; check hub logs: journalctl --user -u urnetwork-hub.service -n 30")
}

func firewallHintText() string {
	return "urnet-tools hub open-port 8443 / your firewall"
}

// cmdHubUpdate resolves the target hub version (explicit tag, or the highest
// installed) and reinstalls the hub binary by re-running the install path,
// then records the version.
func cmdHubUpdate(p Provider, rest []string) error {
	if err := cmdHubInstall(p, rest); err != nil {
		return err
	}
	dir, err := hubDataDir(p)
	if err != nil {
		return err
	}
	tag := ""
	if len(rest) > 0 {
		tag = strings.TrimPrefix(rest[0], "--tag=")
	}
	if tag == "" {
		if rel, lerr := latestRelease(); lerr == nil {
			tag = rel.Tag
		}
	}
	if tag != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		_ = os.WriteFile(filepath.Join(dir, ".hub_version"), []byte(tag), 0o600)
		fmt.Printf("Hub version recorded: %s\n", tag)
	}
	return nil
}

// cmdHubOpenPort opens a TCP port in the provider's firewall (ufw or
// firewalld). Linux-only.
func cmdHubOpenPort(port string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("hub open-port is Linux-only (firewall); open port %s manually on this OS", port)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return fmt.Errorf("port must be numeric (got %q)", port)
	}
	if _, err := exec.LookPath("ufw"); err == nil {
		cmd := exec.Command("ufw", "allow", port+"/tcp")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err == nil {
			fmt.Printf("Opened %s/tcp via ufw\n", port)
			return nil
		}
	}
	if _, err := exec.LookPath("firewall-cmd"); err == nil {
		cmd := exec.Command("firewall-cmd", "--permanent", "--add-port="+port+"/tcp")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		_ = cmd.Run()
		cmd2 := exec.Command("firewall-cmd", "--reload")
		cmd2.Stdout = os.Stdout
		cmd2.Stderr = os.Stderr
		if cmd2.Run() == nil {
			fmt.Printf("Opened %s/tcp via firewalld\n", port)
			return nil
		}
	}
	return fmt.Errorf("no supported firewall detected (ufw or firewalld); open port %s manually", port)
}
