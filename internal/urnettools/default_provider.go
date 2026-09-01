// default_provider.go — a persisted "default provider" selector so a
// multi-provider box can be used without re-picking the target on every
// command. An operator opts in explicitly (they run `default set`); nothing
// auto-guesses. The default is resolved by the stable network-id when
// available, else user+network+state-dir, so a renamed unit never breaks the
// match. It only applies when NO explicit target flag is given AND multiple
// providers exist (the case that currently triggers the interactive picker /
// ambiguity refusal). Explicit targets and single-provider boxes are
// unchanged.

package urnettools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultProviderFile is the per-user config file holding the persisted
// default target selector. Stored under the user config dir so it works on
// Linux/macOS/Windows.
const DefaultProviderFile = "default"

// defaultConfigDir returns the dir where the tool stores per-user config.
func defaultConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "urnet-tools"), nil
}

// defaultConfigPath is the full path to the persisted default file.
func defaultConfigPath() (string, error) {
	dir, err := defaultConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, DefaultProviderFile), nil
}

// readDefaultProvider loads the persisted default target selector. Returns an
// empty Target if none is set or the file is absent/unreadable (treated as
// "no default"), NOT an error — the absence of a default is normal.
func readDefaultProvider() Target {
	p, err := defaultConfigPath()
	if err != nil {
		return Target{}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return Target{}
	}
	var t Target
	if err := json.Unmarshal(b, &t); err != nil {
		return Target{}
	}
	return t
}

// writeDefaultProvider persists the default target selector, returning the
// path written.
func writeDefaultProvider(t Target) (string, error) {
	dir, err := defaultConfigDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, DefaultProviderFile)
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return "", err
	}
	// M7 fix: atomic write with fsync for crash safety.
	if err := writeFileAtomic(path, b, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// clearDefaultProvider removes the persisted default. It is not an error if
// none was set.
func clearDefaultProvider() error {
	p, err := defaultConfigPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// resolveDefaultProvider applies a persisted default target to a providers
// list when the caller gave NO explicit target. Returns (provider, true) if
// the default resolved to exactly one provider; (Provider{}, false) if there
// is no default or it is ambiguous — the caller then proceeds with its normal
// ambiguity handling. It never guesses: a stale/ambiguous default is ignored,
// not acted on.
func resolveDefaultProvider(providers []Provider) (Provider, bool) {
	t := readDefaultProvider()
	if t.Unit == "" && t.User == "" && t.Network == "" && t.NetworkID == "" && t.StateDir == "" {
		return Provider{}, false
	}
	var matches []Provider
	for _, p := range providers {
		if t.matchProvider(p) {
			matches = append(matches, p)
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return Provider{}, false
}

// cmdDefault manages the persisted default provider target.
//
//	urnet-tools default set --network=NAME
//	urnet-tools default set --user=alice --network=NAME
//	urnet-tools default show
//	urnet-tools default clear
func cmdDefault(args []string) error {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: urnet-tools default <set|show|clear> (set accepts --unit/--user/--network/--network-id/--state-dir)\n")
		return nil
	}
	switch args[0] {
	case "set":
		t, _, err := parseTargetFlagsLenient(args[1:])
		if err != nil {
			return err
		}
		if t.Unit == "" && t.User == "" && t.Network == "" && t.NetworkID == "" && t.StateDir == "" {
			return fmt.Errorf("default set requires a target selector (--unit/--user/--network/--network-id/--state-dir)")
		}
		path, err := writeDefaultProvider(t)
		if err != nil {
			return err
		}
		fmt.Printf("default provider set to %s (stored in %s)\n", t, path)
		return nil
	case "show":
		t := readDefaultProvider()
		if t.Unit == "" && t.User == "" && t.Network == "" && t.NetworkID == "" && t.StateDir == "" {
			fmt.Println("no default provider set")
			return nil
		}
		fmt.Printf("default provider: %s\n", t)
		return nil
	case "clear":
		if err := clearDefaultProvider(); err != nil {
			return err
		}
		fmt.Println("default provider cleared")
		return nil
	default:
		return fmt.Errorf("unknown default subcommand %q (set|show|clear)", args[0])
	}
}
