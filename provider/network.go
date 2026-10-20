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
	// A file that parses but does not name a network (`{}`, `null`, or a
	// half-written one with a single field) is an error here rather than a
	// config: returning it would hand an empty base URL to the api client and
	// fail much further from the cause.
	if cfg.ApiUrl == "" || cfg.ConnectUrl == "" {
		return networkConfig{}, false, fmt.Errorf("%s: api_url and connect_url are both required", p)
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
	// Through a temp file and a rename, so a crash or a full disk cannot leave
	// a truncated network.json behind: every command now reads it, and a bad
	// one stops them all (under a Restart=always unit, permanently).
	return atomicWriteFile(p, b, 0600)
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
// Parses with url.Parse (not manual scheme-trimming + SplitHostPort) so a
// path or query string on the URL (e.g. "https://api.example.com:8443/v1")
// doesn't get swept into the port and silently fall back to the default.
func apiProbeHostPort(apiUrl string) (string, uint16) {
	if apiUrl == "" {
		return defaultAPIHost, uint16(defaultAPIPort)
	}
	u, err := url.Parse(apiUrl)
	if err != nil || u.Hostname() == "" {
		return defaultAPIHost, uint16(defaultAPIPort)
	}
	apiProbeHost := u.Hostname()
	apiProbePort := uint16(defaultAPIPort)
	if p := u.Port(); p != "" {
		if port, err := strconv.Atoi(p); err == nil && port >= 1 && port <= 65535 {
			apiProbePort = uint16(port)
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

// urlScheme returns the scheme of rawUrl, or "" when it does not parse.
func urlScheme(rawUrl string) string {
	u, err := url.Parse(rawUrl)
	if err != nil {
		return ""
	}
	return u.Scheme
}

// insecureNetworkWarning returns a warning when the chosen network would
// carry the provider's credentials in the clear, or "" when both URLs are
// encrypted.
//
// The network jwt travels as an `Authorization: Bearer` header on every api
// call, so a plaintext api_url puts it on the wire in the clear for anyone on
// the path. The one-off --api_url flag has always allowed this, but a saved
// network makes it persistent and invisible, which is why it is worth saying
// out loud at the point it gets written down and whenever it is shown.
func insecureNetworkWarning(apiUrl, connectUrl string) string {
	insecure := []string{}
	if urlScheme(apiUrl) == "http" {
		insecure = append(insecure, "api_url uses http")
	}
	if urlScheme(connectUrl) == "ws" {
		insecure = append(insecure, "connect_url uses ws")
	}
	if len(insecure) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"warning: %s. Traffic to this network is unencrypted, including the provider's auth token, "+
			"and is readable by anyone on the network path. Use https:// and wss:// unless this is a "+
			"local test deployment.",
		strings.Join(insecure, " and "),
	)
}

// apiUrlChanged reports whether selecting newApiUrl changes the network the
// provider talks to. With no saved config the provider is already on
// DefaultApiUrl, so writing the default back (or resetting to it) is not a
// switch. An unreadable config cannot be compared, so it counts as a change:
// better a superfluous warning than a jwt silently pointed at another network.
func apiUrlChanged(newApiUrl string) bool {
	cfg, ok, err := readNetworkConfig()
	if err != nil {
		return true
	}
	if !ok {
		return newApiUrl != DefaultApiUrl
	}
	return newApiUrl != cfg.ApiUrl
}

// networkSwitchNotice warns that the credentials already in dir belong to the
// previously selected network, or returns "" when there are none or when the
// effective network did not move.
//
// The jwt is minted by one network's api and means nothing to another, so
// after a switch `provider provide` fails with an auth error whose cause is
// several steps removed from the message.
//
// changed is the caller's comparison of the effective api_url before and
// after the command. A save or reset that leaves the network where it was is
// not a switch — the jwt still belongs to the selected network — and the
// notice costs a single-use auth code when followed, so it must stay quiet.
func networkSwitchNotice(dir string, changed bool) string {
	if !changed {
		return ""
	}
	if _, err := os.Stat(filepath.Join(dir, "jwt")); err != nil {
		return ""
	}
	return fmt.Sprintf(
		"note: the jwt in %s was issued by the previously selected network. "+
			"Run `provider auth` against the new network before `provider provide`.",
		dir,
	)
}

// networkShowText is the text of `provider choose_network --show`: the saved
// network, or the default one when none is saved, with the insecure-network
// warning when it applies.
func networkShowText() (string, error) {
	cfg, ok, err := readNetworkConfig()
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf(
			"no custom network saved; using the main network\n    api_url: %s\n    connect_url: %s\n",
			DefaultApiUrl, DefaultConnectUrl,
		), nil
	}
	p, err := networkConfigPath()
	if err != nil {
		return "", err
	}
	text := fmt.Sprintf(
		"custom network saved at %s\n    api_url: %s\n    connect_url: %s\n",
		p, cfg.ApiUrl, cfg.ConnectUrl,
	)
	if warning := insecureNetworkWarning(cfg.ApiUrl, cfg.ConnectUrl); warning != "" {
		text += warning + "\n"
	}
	return text, nil
}

// networkSwitchNoticeForState is networkSwitchNotice for the provider state
// directory that holds network.json. changed must say whether the command
// moved the effective network.
func networkSwitchNoticeForState(changed bool) string {
	p, err := networkConfigPath()
	if err != nil {
		return ""
	}
	return networkSwitchNotice(filepath.Dir(p), changed)
}
