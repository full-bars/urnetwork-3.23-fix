package main

// network.go — persisted custom-network selection for the provider CLI.
// `provider choose_network <api_url> <connect_url>` writes the chosen
// network to ~/.urnetwork/network.json (alongside jwt and
// .provider.key, via the existing providerStatePath helper);
// `provider choose_network --reset` removes it. resolveApiUrl and
// resolveConnectUrl apply the flag > saved-config > default precedence
// on top of this file. Ported from urfoundation/sn PR #1
// (miner/network.go).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/docopt/docopt-go"
)

// networkConfig is the on-disk shape of ~/.urnetwork/network.json.
type networkConfig struct {
	ApiUrl     string `json:"api_url"`
	ConnectUrl string `json:"connect_url"`
}

// networkConfigPath returns the absolute path of the saved network
// config, alongside jwt and .provider.key under ~/.urnetwork. Does not
// require the file or the ~/.urnetwork directory to exist.
func networkConfigPath() (string, error) {
	return providerStatePath("network.json")
}

// validateApiUrl requires an http or https URL.
func validateApiUrl(rawUrl string) error {
	u, err := url.Parse(rawUrl)
	if err != nil {
		return fmt.Errorf("invalid api_url %q: %w", rawUrl, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid api_url %q: scheme must be http or https, got %q", rawUrl, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid api_url %q: missing host", rawUrl)
	}
	return nil
}

// validateConnectUrl requires a ws or wss URL.
func validateConnectUrl(rawUrl string) error {
	u, err := url.Parse(rawUrl)
	if err != nil {
		return fmt.Errorf("invalid connect_url %q: %w", rawUrl, err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return fmt.Errorf("invalid connect_url %q: scheme must be ws or wss, got %q", rawUrl, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid connect_url %q: missing host", rawUrl)
	}
	return nil
}

// readNetworkConfig loads the saved network config. ok is false (with a
// nil error) when the file does not exist — a fresh install with no
// custom network saved.
func readNetworkConfig() (cfg networkConfig, ok bool, err error) {
	p, err := networkConfigPath()
	if err != nil {
		return networkConfig{}, false, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return networkConfig{}, false, nil
	}
	if err != nil {
		return networkConfig{}, false, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return networkConfig{}, false, fmt.Errorf("parse %s: %w", p, err)
	}
	return cfg, true, nil
}

// writeNetworkConfig validates apiUrl (http/https) and connectUrl
// (ws/wss), then writes them to ~/.urnetwork/network.json, creating the
// ~/.urnetwork directory if needed. Nothing is written if validation
// fails.
func writeNetworkConfig(apiUrl, connectUrl string) error {
	if err := validateApiUrl(apiUrl); err != nil {
		return err
	}
	if err := validateConnectUrl(connectUrl); err != nil {
		return err
	}
	p, err := networkConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(networkConfig{ApiUrl: apiUrl, ConnectUrl: connectUrl}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0600)
}

// resetNetworkConfig removes ~/.urnetwork/network.json. Removing a
// nonexistent file is not an error.
func resetNetworkConfig() error {
	p, err := networkConfigPath()
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// resolveApiUrl implements the 3-tier precedence for the API URL:
// --api_url flag > saved network config > DefaultApiUrl.
func resolveApiUrl(opts docopt.Opts) (string, error) {
	if apiUrl, err := opts.String("--api_url"); err == nil {
		return apiUrl, nil
	}
	cfg, ok, err := readNetworkConfig()
	if err != nil {
		return "", err
	}
	if ok {
		return cfg.ApiUrl, nil
	}
	return DefaultApiUrl, nil
}

// resolveConnectUrl implements the 3-tier precedence for the connect
// URL: --connect_url flag > saved network config > DefaultConnectUrl.
func resolveConnectUrl(opts docopt.Opts) (string, error) {
	if connectUrl, err := opts.String("--connect_url"); err == nil {
		return connectUrl, nil
	}
	cfg, ok, err := readNetworkConfig()
	if err != nil {
		return "", err
	}
	if ok {
		return cfg.ConnectUrl, nil
	}
	return DefaultConnectUrl, nil
}

// apiProbeHostPort extracts the API probe host:port from an API URL,
// falling back to defaultAPIHost/defaultAPIPort for URLs that don't
// parse into a host. Shared by provide()'s reachability-probe setup and
// `proxy add-source`'s one-shot fetch so both follow the chosen network.
func apiProbeHostPort(apiUrl string) (string, uint16) {
	apiProbeHost := defaultAPIHost
	apiProbePort := uint16(defaultAPIPort)
	if apiUrl != "" {
		if h, p, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(apiUrl, "https://"), "http://")); err == nil {
			apiProbeHost = h
			if port, err := strconv.Atoi(p); err == nil && port >= 1 && port <= 65535 {
				apiProbePort = uint16(port)
			}
		} else {
			// No port in URL, just a hostname
			cleaned := strings.TrimPrefix(strings.TrimPrefix(apiUrl, "https://"), "http://")
			if cleaned != "" {
				apiProbeHost = cleaned
			}
		}
	}
	return apiProbeHost, apiProbePort
}

// resolveAPIProbeHostPort resolves the API probe endpoint from the chosen
// network (saved network config > default), for call sites that run without
// docopt opts (e.g. the `proxy add-source` one-shot fetch).
func resolveAPIProbeHostPort() (string, uint16) {
	apiUrl := DefaultApiUrl
	if cfg, ok, err := readNetworkConfig(); err == nil && ok && cfg.ApiUrl != "" {
		apiUrl = cfg.ApiUrl
	}
	return apiProbeHostPort(apiUrl)
}
