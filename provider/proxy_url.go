package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProxyURLState is the on-disk record of configured live proxy URL sources
// and the addresses fetched from them so far. Unlike proxy.state, this file
// is additive-only by design: fetched addresses are only ever removed by
// removeDeadProxies (manual or automatic cleanup), never by a fetch cycle.
type ProxyURLState struct {
	Sources []string                 `json:"sources"`
	Cache   map[string]ProxyURLEntry `json:"cache"`
}

// ProxyURLEntry records the auth (if any) for one address fetched from a URL
// source. Most public proxy lists provide unauthenticated entries.
type ProxyURLEntry struct {
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
}

func proxyURLStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy_url.json"), nil
}

func readProxyURLState() (*ProxyURLState, error) {
	path, err := proxyURLStatePath()
	if err != nil {
		return nil, err
	}
	return readProxyURLStateFrom(path)
}

func readProxyURLStateFrom(path string) (*ProxyURLState, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &ProxyURLState{Cache: map[string]ProxyURLEntry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read proxy_url.json: %w", err)
	}
	var s ProxyURLState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse proxy_url.json: %w", err)
	}
	if s.Cache == nil {
		s.Cache = map[string]ProxyURLEntry{}
	}
	return &s, nil
}

func writeProxyURLState(s *ProxyURLState) error {
	path, err := proxyURLStatePath()
	if err != nil {
		return err
	}
	return writeProxyURLStateTo(path, s)
}

func writeProxyURLStateTo(path string, s *ProxyURLState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// parseProxyURLLine parses one line from a remote proxy list. Unlike
// parseProxyAddress (used by --proxy_file, which requires credentials),
// entries without credentials are valid here — open/anonymous proxies are
// the common case for public proxy lists. Accepted forms:
//
//	host:port
//	host:port:user:pass
//	socks5://host:port
//	socks5://user:pass@host:port
//
// Returns ok=false if the line is blank, a comment, or uses an unsupported
// protocol scheme (this fork is SOCKS5-only).
func parseProxyURLLine(line string) (address, user, password string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return "", "", "", false
	}

	if idx := strings.Index(line, "://"); idx != -1 {
		scheme := line[:idx]
		if !strings.EqualFold(scheme, "socks5") {
			fmt.Printf("[proxy][url] unsupported scheme %q (only socks5 is supported); skipping %q\n", scheme, line)
			return "", "", "", false
		}
		rest := line[idx+3:]
		if at := strings.LastIndex(rest, "@"); at != -1 {
			cred := rest[:at]
			address = rest[at+1:]
			if parts := strings.SplitN(cred, ":", 2); len(parts) == 2 {
				user, password = parts[0], parts[1]
			}
			return address, user, password, true
		}
		address, user, password = parseProxyAddress(rest)
		return address, user, password, true
	}

	address, user, password = parseProxyAddress(line)
	return address, user, password, true
}
