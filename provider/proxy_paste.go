package main

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/docopt/docopt-go"
)

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
		return fmt.Sprintf("%s::", addrPart)
	}

	// Format 2: host:port:user:pass (already canonical)
	r := regexp.MustCompile(`^(.*:\d+):([^:]*):([^:]*)$`)
	if groups := r.FindStringSubmatch(line); groups != nil {
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
		if port == "" || !regexp.MustCompile(`^\d+$`).MatchString(port) {
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
	if port == "" || !regexp.MustCompile(`^\d+$`).MatchString(port) {
		return ""
	}
	host := strings.Join(parts[:len(parts)-1], ":")
	if host == "" {
		return ""
	}
	// If the host itself contains a colon (IPv6 like ::1 with exactly 3
	// segments: ["", "", "1"] + port), it needs brackets to be dialable.
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
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
	client := &http.Client{Timeout: 30 * time.Second}
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
		if csvResult := parseCSVProxyList(content); len(csvResult) > 0 {
			return csvResult
		}
	}

	// Line-by-line (txt format)
	var result []string
	for _, line := range lines {
		if n := normalizeProxyLine(line); n != "" {
			result = append(result, n)
		}
	}
	return result
}

func proxyPaste(opts docopt.Opts) {
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
		}
	}

	// Fetch and parse URL sources
	for _, url := range sourceURLs {
		fmt.Printf("fetching %s ... ", url)
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
		return
	}

	// Write normalized proxies to temp file
	tmpFile, err := os.CreateTemp("", "proxy-paste-*.txt")
	if err != nil {
		shmLogFatal(81, "could not create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	for _, line := range allNormalized {
		fmt.Fprintln(tmpFile, line)
	}
	tmpFile.Close()

	fmt.Printf("\nadding %d proxies (%d dupes skipped, %d rejected)\n",
		len(allNormalized), totalSkipped, totalRejected)

	// Find our own binary path
	bin, err := os.Executable()
	if err != nil {
		shmLogFatal(82, "cannot determine executable path: %v", err)
	}

	// proxy add --proxy_file=<tmp> -f
	addCmd := exec.Command(bin, "proxy", "add", "--proxy_file="+tmpFile.Name(), "-f")
	addCmd.Stdout = os.Stdout
	addCmd.Stderr = os.Stderr
	if err := addCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "proxy add failed: %v\n", err)
		os.Exit(1)
	}

	// proxy refresh --force
	// proxyRefresh requires an interactive confirm("Proceed?") whenever the
	// reload would add/remove proxies — --force only bypasses the warmup gate,
	// NOT the prompt (main.go proxyRefresh). Without stdin wired, the child
	// reads /dev/null, confirm() gets EOF -> false, prints "Aborted." and
	// returns exit 0 — the reload trigger is never written and the running
	// provider never sees the new proxies. Pipe the confirmation.
	fmt.Println("refreshing...")
	refreshCmd := exec.Command(bin, "proxy", "refresh", "--force")
	refreshCmd.Stdin = strings.NewReader("y\ny\n")
	refreshCmd.Stdout = os.Stdout
	refreshCmd.Stderr = os.Stderr
	if err := refreshCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "proxy refresh failed: %v\n", err)
		os.Exit(1)
	}
}

func readLines(r io.Reader) []string {
	var lines []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines
}
