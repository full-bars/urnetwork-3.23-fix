package main

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"
)

// canonicalProxyRe matches the already-canonical host:port:user:pass form.
// Compiled once at package load (hoisted out of normalizeProxyLine, which runs
// per pasted line — a large list otherwise pays a compile per entry).
var canonicalProxyRe = regexp.MustCompile(`^(.*:\d+):([^:]*):([^:]*)$`)

// normalizeProxyLine parses a single line of proxy input in common SOCKS5
// formats and returns the urnetwork canonical form: host:port:user:pass
// or host:port:: (empty auth). Returns empty string if unparseable.
func normalizeProxyLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
		return ""
	}

	// Strip socks5:// prefix; reject socks4 and http/https as proxy formats
	lower := strings.ToLower(line)
	if strings.HasPrefix(lower, "socks5://") {
		line = line[len("socks5://"):]
	} else if strings.HasPrefix(lower, "socks4://") ||
		strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") {
		return ""
	}

	// Strip trailing path/fragment: host:port/path?query
	if idx := strings.IndexAny(line, "/?#"); idx >= 0 {
		line = line[:idx]
	}

	// Format 1: user:pass@host:port (or key@host:port)
	if atIdx := strings.LastIndex(line, "@"); atIdx > 0 {
		credPart := line[:atIdx]
		addrPart := line[atIdx+1:]
		addrPart = normalizeHostPort(addrPart)
		if addrPart == "" {
			return ""
		}
		if colonIdx := strings.Index(credPart, ":"); colonIdx >= 0 {
			user := credPart[:colonIdx]
			pass := credPart[colonIdx+1:]
			// The canonical host:port:user:pass form cannot represent
			// colons inside credentials: parseProxyAddress (main.go) splits
			// on the last two colons and would misparse or drop them,
			// producing a proxy that silently never authenticates. Reject
			// loudly instead of corrupting.
			if strings.Contains(user, ":") || strings.Contains(pass, ":") {
				return ""
			}
			return fmt.Sprintf("%s:%s:%s", addrPart, user, pass)
		}
		// Single-token credential (key@host:port): the key is an API token
		// that authenticates the proxy, so dropping it silently would yield
		// a proxy that never authenticates. Store it in the user field,
		// matching the documented Format 1 behaviour.
		return fmt.Sprintf("%s:%s:", addrPart, credPart)
	}

	// Format 2: host:port:user:pass (already canonical)
	if groups := canonicalProxyRe.FindStringSubmatch(line); groups != nil {
		addr := normalizeHostPort(groups[1])
		if addr == "" {
			return ""
		}
		return fmt.Sprintf("%s:%s:%s", addr, groups[2], groups[3])
	}

	// Format 3: host:port user:pass (space-separated)
	parts := strings.Fields(line)
	if len(parts) == 2 {
		addr := normalizeHostPort(parts[0])
		if addr == "" {
			return ""
		}
		cred := parts[1]
		if colonIdx := strings.Index(cred, ":"); colonIdx >= 0 {
			user := cred[:colonIdx]
			pass := cred[colonIdx+1:]
			// Colon-in-credential guard, same as Format 1.
			if strings.Contains(user, ":") || strings.Contains(pass, ":") {
				return ""
			}
			return fmt.Sprintf("%s:%s:%s", addr, user, pass)
		}
		return fmt.Sprintf("%s::", addr)
	}

	// Format 4: host:port (bare)
	if addr := normalizeHostPort(line); addr != "" {
		return fmt.Sprintf("%s::", addr)
	}

	return ""
}

// normalizeHostPort validates and returns host:port. Returns "" if invalid.
// Hosts containing multiple colons (IPv6) are returned BRACKETED —
// [::1]:1080 — because net.SplitHostPort (used by every dial path) rejects
// unbracketed IPv6 with "too many colons in address". Bare unbracketed IPv6
// input like `::1:1080` is ambiguous (last hextet vs port) and is rejected.
func normalizeHostPort(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Bracketed IPv6 input: [host]:port
	if strings.HasPrefix(s, "[") {
		closeIdx := strings.Index(s, "]")
		if closeIdx < 0 {
			return ""
		}
		host := s[1:closeIdx]
		if host == "" || strings.Contains(host, "[") || strings.Contains(host, "]") {
			return ""
		}
		rest := s[closeIdx+1:]
		if !strings.HasPrefix(rest, ":") {
			return ""
		}
		port := rest[1:]
		if !validPort(port) {
			return ""
		}
		return "[" + host + "]:" + port
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 {
		return ""
	}
	// Unbracketed IPv6 (3+ colon-separated segments where the host part has
	// colons): ambiguous with host:port parsing — reject rather than guess.
	if len(parts) > 3 {
		return ""
	}
	port := parts[len(parts)-1]
	if !validPort(port) {
		return ""
	}
	host := strings.Join(parts[:len(parts)-1], ":")
	if host == "" {
		return ""
	}
	// If the host itself contains a colon (IPv6 like ::1 with exactly 3
	// segments: ["", "", "1"] + port), it needs brackets to be dialable. Only
	// bracket when the colon-host is actually a valid IPv6 literal — otherwise
	// a malformed input like host:1.2.3.4 (cred-free @-form with a colon-ish
	// remainder, or plain garbage) would be accepted as a pseudo-IPv6 and
	// produce an address that will simply never dial.
	if strings.Contains(host, ":") {
		if net.ParseIP(host) == nil {
			return ""
		}
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// validPort reports whether s is a decimal TCP/UDP port in the valid range
// 1-65535. Port 0 is not a routable destination and >65535 is out of range —
// both would produce a proxy that can never connect.
func validPort(s string) bool {
	if s == "" || len(s) > 5 {
		return false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return false
	}
	return n >= 1 && n <= 65535
}

// isSourceURL returns true if the line is an HTTP/HTTPS URL (proxy source).
func isSourceURL(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// fetchURL fetches a URL and returns its body as a string.
// Body is capped at 10 MiB, matching the proxy-URL-source fetch convention
// (io.LimitReader) — an infinite/huge response must not exhaust memory.
func fetchURL(url string) (string, error) {
	// Use the hardened client (SSRF guard + redirect limit), same trust model
	// as the `proxy add-source` fetch path.
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: ssrfCheckRedirect, Transport: sourceURLTransport()}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// parseCSVProxyList parses CSV content into proxy lines.
// Supports: ip,port,user,pass | ip,port | host:port:user:pass (single column)
// Reads row-by-row with variable field counts so one malformed row is
// skipped, not fatal for the whole batch.
func parseCSVProxyList(content string) []string {
	var result []string
	reader := csv.NewReader(strings.NewReader(content))
	reader.FieldsPerRecord = -1 // variable columns per row
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Malformed row (unterminated quote etc.) — skip it, keep going.
			continue
		}
		var line string
		switch len(record) {
		case 4:
			line = fmt.Sprintf("%s:%s:%s:%s", record[0], record[1], record[2], record[3])
		case 3:
			line = fmt.Sprintf("%s:%s:%s:", record[0], record[1], record[2])
		case 2:
			line = fmt.Sprintf("%s:%s", record[0], record[1])
		case 1:
			line = record[0]
		default:
			continue
		}
		if n := normalizeProxyLine(line); n != "" {
			result = append(result, n)
		}
	}
	return result
}

// parseProxyList parses text content (txt or csv) into normalized proxy lines.
func parseProxyList(content string) []string {
	lines := strings.Split(content, "\n")

	// Heuristic: if >50% of non-empty lines have commas, treat as CSV
	commaCount := 0
	totalNonEmpty := 0
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			totalNonEmpty++
			if strings.Contains(l, ",") {
				commaCount++
			}
		}
	}
	if totalNonEmpty > 0 && commaCount > totalNonEmpty/2 {
		csvResult := parseCSVProxyList(content)
		if len(csvResult) > 0 {
			// The comma heuristic can misfire on TXT lists whose credentials
			// happen to contain a comma — the CSV parser then mangles those
			// lines and (worse) the old code returned csvResult unconditionally,
			// silently dropping the mangled entries. Union with the line-by-line
			// parser and keep whichever recovers more, so a legitimate TXT list
			// never loses entries to the heuristic. The dedup map below is only
			// seeded from the CSV pass first so true-CSV wins the tie.
			txtResult := parseProxyListLineByLine(lines)
			seen := make(map[string]struct{}, len(txtResult)+len(csvResult))
			for _, l := range csvResult {
				seen[l] = struct{}{}
			}
			for _, l := range txtResult {
				if _, dup := seen[l]; dup {
					continue
				}
				seen[l] = struct{}{}
				csvResult = append(csvResult, l)
			}
			return csvResult
		}
	}

	// Line-by-line (txt format)
	return parseProxyListLineByLine(lines)
}

// parseProxyListLineByLine parses txt content one line at a time.
func parseProxyListLineByLine(lines []string) []string {
	// Line-by-line (txt format)
	var result []string
	for _, line := range lines {
		if n := normalizeProxyLine(line); n != "" {
			result = append(result, n)
		}
	}
	return result
}

// redactProxyLine strips credential material from a rejected line for safe
// operator diagnostics. Rejected input is exactly the class most likely to
// contain a live password (malformed credential syntax); echoing it verbatim
// leaks it into scrollback/logs. Best-effort: strip a {user}:{pass}@ prefix
// (or a {user}:{pass} tail) and show only the host:port-ish residue.
func redactProxyLine(line string) string {
	s := strings.TrimSpace(line)
	// user:pass@host:port → keep the part after the last '@'.
	// A bare trailing '@' means the line is `user:pass@` with no host —
	// there is no suffix to keep, so return a redaction placeholder
	// rather than leaking the credential verbatim.
	if at := strings.LastIndex(s, "@"); at >= 0 {
		if at == len(s)-1 {
			return "<redacted>"
		}
		s = s[at+1:]
	}
	// host:port user:pass (space-separated) → keep only the first token; the
	// second token is the credential we're hiding.
	if parts := strings.Fields(s); len(parts) == 2 {
		s = parts[0]
	}
	// host:port:user:pass → keep the addr (first two colon-separated fields).
	// Don't split on a third colon, since the tail is the credential we're hiding.
	if parts := strings.SplitN(s, ":", 3); len(parts) >= 2 && validColonForm(s) {
		s = parts[0] + ":" + parts[1]
	}
	return s
}

// validColonForm reports whether s looks like host:port[:cred] for redaction
// purposes (vs a bare hostname or an already-credential-stripped host:port).
func validColonForm(s string) bool {
	parts := strings.Split(s, ":")
	// host:port (2 fields) needs no further redaction; host:port:cred has 3+.
	return len(parts) >= 3
}

func proxyPaste(opts docopt.Opts) {
	// Reject paste for file-backed providers: when the provider runs with
	// --proxy_file=/PROXY_FILE=<X> (Workflow A), it loads proxies from that
	// external file on start, so a pasted entry written to the internal
	// proxyConfig.Servers would be lost on the next reload. Pasting is only
	// meaningful for the internal-config (Workflow B) model.
	if state, err := readProxyState(); err == nil && state.Source != "" {
		fmt.Fprintf(os.Stderr, "this provider is file-backed (--proxy_file=%s); paste writes the internal config and would be lost on reload — add the entries to that file instead (or use 'proxy add --proxy_file=...'), or re-run the provider without a file source to paste.\n", state.Source)
		os.Exit(1)
	}

	// Determine input source
	var rawLines []string

	if filePath, _ := opts.String("--file"); filePath != "" {
		f, err := os.Open(filePath)
		if err != nil {
			shmLogFatal(80, "could not open file %s: %v", filePath, err)
		}
		defer f.Close()
		rawLines = readLines(f)
	} else {
		if stat, _ := os.Stdin.Stat(); (stat.Mode() & os.ModeCharDevice) != 0 {
			fmt.Fprintln(os.Stderr, "Paste proxies + source URLs, then Ctrl+D when done:")
		}
		rawLines = readLines(os.Stdin)
	}

	if len(rawLines) == 0 {
		fmt.Println("no input received")
		return
	}

	// Separate URLs from direct proxies
	var sourceURLs []string
	var directLines []string
	for _, line := range rawLines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if isSourceURL(trimmed) {
			sourceURLs = append(sourceURLs, trimmed)
		} else {
			directLines = append(directLines, trimmed)
		}
	}

	// Dedup: direct lines are added FIRST, so a proxy present both pasted
	// directly and inside a fetched source list keeps the direct entry and
	// the fetched copy counts as skipped. Deliberate ordering — lock it in
	// with TestDedupDirectWins.
	seen := map[string]bool{}
	var allNormalized []string
	var totalSkipped, totalRejected int
	var rejectedLines []string // first N rejected lines for diagnostics
	const maxRejectedShow = 10

	// Process direct proxy lines
	for _, line := range directLines {
		if n := normalizeProxyLine(line); n != "" {
			if !seen[n] {
				seen[n] = true
				allNormalized = append(allNormalized, n)
			} else {
				totalSkipped++
			}
		} else {
			totalRejected++
			if len(rejectedLines) < maxRejectedShow {
				rejectedLines = append(rejectedLines, strings.TrimSpace(line))
			}
		}
	}

	// Fetch and parse URL sources
	for _, url := range sourceURLs {
		fmt.Printf("fetching %s ... ", sanitizeURLForDisplay(url))
		content, err := fetchURL(url)
		if err != nil {
			fmt.Printf("error: %v\n", err)
			continue
		}
		proxies := parseProxyList(content)
		added := 0
		for _, p := range proxies {
			if !seen[p] {
				seen[p] = true
				allNormalized = append(allNormalized, p)
				added++
			} else {
				totalSkipped++
			}
		}
		fmt.Printf("%d proxies\n", added)
	}

	if len(allNormalized) == 0 {
		fmt.Printf("no valid proxies found (%d skipped, %d rejected)\n", totalSkipped, totalRejected)
		for _, rl := range rejectedLines {
			fmt.Fprintf(os.Stderr, "  rejected: %s\n", redactProxyLine(rl))
		}
		return
	}

	// Write normalized proxies to temp file
	tmpFile, err := os.CreateTemp("", "proxy-paste-*.txt")
	if err != nil {
		shmLogFatal(81, "could not create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	// os.Exit and shmLogFatal bypass deferred calls, so the temp file holding
	// plaintext proxy credentials would be left on disk on any error path
	// below. Remove it explicitly at each exit point.
	removeTempFile := func() {
		if tmpFile != nil {
			os.Remove(tmpFile.Name())
		}
	}

	// An external kill (operator Ctrl+C, systemd/orchestration SIGTERM) mid-paste
	// would otherwise leave the plaintext temp file on disk — os.Exit & shmLogFatal
	// bypass the deferred Remove, and there's no signal handler to catch it. Register
	// a handler for the window the file is alive: remove the file, then restore the
	// default disposition and re-raise the signal so the process exits with the
	// conventional status. signal.Notify suppresses the default terminate, so we must
	// signal.Reset(sig) first or the re-raised signal would just be caught again.
	//
	// Registered immediately after the (empty) file is created and BEFORE
	// any plaintext credential line is written -- registering after the
	// write loop left a real window where a SIGINT/SIGTERM arriving mid-write
	// (or between the write loop and this point) would leave plaintext
	// credentials on disk with no handler armed to clean them up.
	cleanupSig := setupPasteSignalHandler(removeTempFile)
	defer cleanupSig()

	for _, line := range allNormalized {
		if _, err := fmt.Fprintln(tmpFile, line); err != nil {
			tmpFile.Close()
			os.Remove(tmpFile.Name())
			shmLogFatal(81, "could not write temp file: %v", err)
		}
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpFile.Name())
		shmLogFatal(81, "could not finalize temp file: %v", err)
	}

	fmt.Printf("\nadding %d proxies (%d dupes skipped, %d rejected)\n",
		len(allNormalized), totalSkipped, totalRejected)
	if len(rejectedLines) > 0 {
		fmt.Fprintln(os.Stderr, "rejected lines:")
		for _, rl := range rejectedLines {
			fmt.Fprintf(os.Stderr, "  %s\n", redactProxyLine(rl))
		}
		if totalRejected > maxRejectedShow {
			fmt.Fprintf(os.Stderr, "  ... and %d more\n", totalRejected-maxRejectedShow)
		}
	}

	// Find our own binary path
	bin, err := os.Executable()
	if err != nil {
		removeTempFile()
		shmLogFatal(82, "cannot determine executable path: %v", err)
	}

	// proxy add --proxy_file=<tmp> -f
	addCmd := exec.Command(bin, "proxy", "add", "--proxy_file="+tmpFile.Name(), "-f")
	addCmd.Stdout = os.Stdout
	addCmd.Stderr = os.Stderr
	if err := addCmd.Run(); err != nil {
		removeTempFile()
		fmt.Fprintf(os.Stderr, "proxy add failed: %v\n", err)
		os.Exit(1)
	}

	// Apply the additions immediately WITHOUT a destructive confirm. The
	// full `proxy refresh --force` path prompts "Remove them anyway?" /
	// "Are you sure?" when its reconcile sees removals — piping "y\ny\n"
	// would auto-confirm a destructive removal, which paste must never do
	// (paste is additive-only). Writing the reload trigger directly applies
	// the just-added proxies without any confirm path. writeReloadTrigger is
	// the same mechanism proxyRefresh uses when its reload path is reached.
	fmt.Println("refreshing...")
	reloadPath, err := proxyReloadPath()
	if err != nil {
		removeTempFile()
		fmt.Fprintf(os.Stderr, "proxy refresh failed: %v\n", err)
		os.Exit(1)
	}
	if err := writeReloadTrigger(reloadPath); err != nil {
		removeTempFile()
		fmt.Fprintf(os.Stderr, "proxy refresh failed: %v\n", err)
		os.Exit(1)
	}
}

func readLines(r io.Reader) []string {
	var lines []string
	scanner := bufio.NewScanner(r)
	// Bump the token buffer well past the 64KB default — a single long line
	// (e.g. an accidentally-unwrapped CSV blob or a list pasted without
	// newlines) would otherwise hit bufio.ErrTooLong, silently abort the scan,
	// and drop that line plus everything after it with no diagnostic.
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: readLines failed partway: %v (input may be truncated)\n", err)
	}
	return lines
}

// sanitizeURLForDisplay strips credential material (userinfo) and the query
// string from a URL before echoing it, so an API token or auth key embedded as
// a query parameter (common for proxy-list providers) never lands in operator
// scrollback or logs. It returns the original string unmodified if the URL
// doesn't parse, since that case won't be fetched anyway.
func sanitizeURLForDisplay(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		// Don't echo the raw input — it may carry userinfo or a sensitive
		// fragment that the caller pasted. Return a safe placeholder.
		return "<invalid source URL>"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// setupPasteSignalHandler registers a SIGINT/SIGTERM notification for the
// duration of the paste operation to remove the plaintext temp file if interrupted.
func setupPasteSignalHandler(removeTempFile func()) func() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	signalHandled := make(chan struct{})
	go func() {
		select {
		case sig := <-sigCh:
			if removeTempFile != nil {
				removeTempFile()
			}
			fmt.Fprintln(os.Stderr, "\ninterrupted; removed proxy-paste temp file")
			signal.Stop(sigCh)
			signal.Reset(sig)
			if p, err := os.FindProcess(os.Getpid()); err == nil {
				_ = p.Signal(sig)
			}
			os.Exit(1)
		case <-signalHandled:
		}
	}()
	return func() {
		signal.Stop(sigCh)
		close(signalHandled)
	}
}
