package urnettools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"
)

// clientJWTEntryMinimal mirrors the subset of clientJWTEntry fields we read
// from the JSON store. Defined here rather than importing provider/ to keep
// urnettools decoupled from the provider package.
type clientJWTEntryMinimal struct {
	ClientID  string    `json:"client_id"`
	NetworkID string    `json:"network_id"`
	MintedAt  time.Time `json:"minted_at"`
}

// renderProxyIDs prints a table of proxy address → client_id, network_id, age
// from the supplied entries (already loaded by the caller from .client_jwts.json).
// For "direct" entries, the proxy address is shown as "(direct)".
func renderProxyIDs(w *tabwriter.Writer, entries map[string]clientJWTEntryMinimal) (err error) {
	defer func() {
		if ferr := w.Flush(); ferr != nil && err == nil {
			err = ferr
		}
	}()
	if len(entries) == 0 {
		fmt.Fprintln(w, "(no entries)")
		return
	}
	// Stable sort by proxy address for deterministic output.
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Fprintln(w, "PROXY	CLIENT_ID	NETWORK_ID	AGE")
	for _, k := range keys {
		e := entries[k]
		label := k
		if label == "direct" {
			label = "(direct)"
		}
		// Show full client_id — this subcommand exists specifically to
		// inspect and correlate client identities.
		cid := e.ClientID
		nidRunes := []rune(e.NetworkID)
		nid := e.NetworkID
		if len(nidRunes) > 8 {
			nid = string(nidRunes[:8]) + "…"
		}
		var age string
		if e.MintedAt.IsZero() {
			age = "-"
		} else {
			age = formatDuration(time.Since(e.MintedAt))
		}
		fmt.Fprintf(w, "%s	%s	%s	%s\n", label, cid, nid, age)
	}
	return
}

// formatDuration renders a duration in human-friendly form: seconds if
// <1m, minutes if <1h, hours if <48h, days otherwise.
func formatDuration(d time.Duration) string {
	if d < 0 {
		return "0s ago"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// loadClientJWTStore reads .client_jwts.json from the given directory and
// returns its parsed entries. Returns an empty map (not nil) when the file is
// missing. Read errors (e.g. permission denied) are propagated and not
// treated as an empty store — this avoids masking access failures as "no
// entries".
func loadClientJWTStore(stateDir string) (map[string]clientJWTEntryMinimal, error) {
	path := filepath.Join(stateDir, ".client_jwts.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]clientJWTEntryMinimal{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var raw map[string]struct {
		ClientID  string    `json:"client_id"`
		NetworkID string    `json:"network_id"`
		MintedAt  time.Time `json:"minted_at"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := make(map[string]clientJWTEntryMinimal, len(raw))
	for k, v := range raw {
		out[k] = clientJWTEntryMinimal{
			ClientID:  v.ClientID,
			NetworkID: v.NetworkID,
			MintedAt:  v.MintedAt,
		}
	}
	return out, nil
}

// cmdProxyIDsTarget prints client IDs for each proxy in the targeted
// provider's client JWT store.
func cmdProxyIDsTarget(p Provider) error {
	stateDir := p.StateDir
	if stateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	entries, err := loadClientJWTStore(stateDir)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintf(os.Stderr, "Client IDs for %s (from %s/.client_jwts.json):\n", providerLabel(p), stateDir)
	if err := renderProxyIDs(w, entries); err != nil {
		return err
	}
	return nil
}
