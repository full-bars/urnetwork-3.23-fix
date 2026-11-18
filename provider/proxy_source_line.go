package main

import (
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

type proxySourceRejection struct {
	line   int
	reason string
}

// classifyProxySourceLine accepts authenticated source entries and ignores comments.
func classifyProxySourceLine(line string) (*connect.ProxySettings, string) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return nil, ""
	}
	var host, port string
	var fields []string
	if strings.HasPrefix(line, "[") {
		end := strings.IndexByte(line, ']')
		if end < 0 || end+1 >= len(line) || line[end+1] != ':' {
			return nil, "invalid_address"
		}
		host = line[1:end]
		if ip := net.ParseIP(host); ip == nil || !strings.Contains(host, ":") {
			return nil, "invalid_address"
		}
		fields = strings.Split(line[end+2:], ":")
	} else {
		parts := strings.Split(line, ":")
		if len(parts) == 0 {
			return nil, "invalid_address"
		}
		host, fields = parts[0], parts[1:]
	}
	if len(fields) != 3 {
		return nil, "invalid_fields"
	}
	port = fields[0]
	if host == "" || strings.TrimSpace(host) != host || strings.ContainsAny(host, " 	\r\n[]@/\\?#") {
		return nil, "invalid_host"
	}
	if port == "" {
		return nil, "invalid_port"
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return nil, "invalid_port"
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, "invalid_port"
	}
	if fields[1] == "" || fields[2] == "" {
		return nil, "missing_credentials"
	}
	return &connect.ProxySettings{Network: "tcp", Address: net.JoinHostPort(host, port), Auth: &proxy.Auth{User: fields[1], Password: fields[2]}}, ""
}

func parseProxySource(data []byte) ([]*connect.ProxySettings, []proxySourceRejection) {
	var settings []*connect.ProxySettings
	var rejected []proxySourceRejection
	for i, line := range strings.Split(string(data), "\n") {
		entry, reason := classifyProxySourceLine(line)
		if reason != "" {
			rejected = append(rejected, proxySourceRejection{i + 1, reason})
		} else if entry != nil {
			settings = append(settings, entry)
		}
	}
	return settings, rejected
}

var proxySourceReports = struct {
	sync.Mutex
	digests map[string][32]byte
}{digests: make(map[string][32]byte)}

func readClassifiedProxySource(path string) ([]*connect.ProxySettings, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("could not read proxy file %s: %w", path, err)
	}
	settings, rejected := parseProxySource(data)
	digest := sha256.Sum256(data)
	proxySourceReports.Lock()
	old, exists := proxySourceReports.digests[path]
	if !exists || old != digest {
		proxySourceReports.digests[path] = digest
		for _, r := range rejected {
			tlog("[proxy] source line %d rejected: %s\n", r.line, r.reason)
		}
	}
	proxySourceReports.Unlock()
	return settings, len(rejected), nil
}
