package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// proxyStateMu serializes all proxy.state read-modify-write cycles.
// Held during: heartbeat snapshot goroutine, reload() state write.
// Not needed for startup writes (heartbeat not yet running).
var proxyStateMu sync.Mutex

// proxyStateVersion is bumped whenever ProxyState.Proxies' map key changes
// format. 2 = keyed by proxy identity (ProxySettings.Key(): address, or
// address+user for a shared-gateway proxy like Decodo). Absent or 1 = the
// legacy format, keyed by bare address alone. writeProxyState always stamps
// the current version; a file with no "version" field (or 0/1) unmarshals
// with Version==0, which adoptLegacyProxyState and every reader treat as
// "may contain legacy bare-address keys, never rewrite or drop them
// blindly" — see adoptLegacyProxyState.
const proxyStateVersion = 2

// ProxyState is the on-disk record of what the provider is currently running.
// Written atomically at startup and after each reload.
type ProxyState struct {
	Version   int                   `json:"version,omitempty"`
	Source    string                `json:"source"`     // live source file path ("" = internal config)
	StartedAt time.Time             `json:"started_at"` // provider process start time
	NextID    int                   `json:"next_id"`    // snapshot of counter for display
	Proxies   map[string]ProxyEntry `json:"proxies"`    // identity key -> entry (see proxyStateVersion)
}

// proxyHealthParked is the Health of a proxy proxy audit is holding out.
// It is distinct from "inactive" on purpose: "inactive" means unseen for 7+ days
// and is collected for removal by dead-proxy cleanup and `proxy remove-dead`,
// which edit the operator's proxy file. A parked proxy is resting and comes
// back on its own, so nothing that removes proxies may treat it as dead.
const proxyHealthParked = "parked"

// ProxyEntry records the stable ID and last-known health for one proxy.
type ProxyEntry struct {
	ID           int    `json:"id"`
	Health       string `json:"health"`                  // "up", "dead", "recently_offline", "offline", "long_offline", "inactive", "parked"
	DownSince    string `json:"down_since,omitempty"`    // RFC3339, set when not up
	Source       string `json:"source,omitempty"`        // "file", "internal", or "url" — where this address was first added from
	AuthFailures int64  `json:"auth_failures,omitempty"` // cumulutive auth errors this run

	// Score is the stage-1 table probe result (ok/total) from the last
	// graded pass, 0 when the entry has never been graded. Mirrors the URL
	// store's ProxyURLEntry fields so fleet grading consumes both stores
	// uniformly. Written ONLY by the paid/file-proxy grading sweep. The
	// admission and eviction paths never read or write these fields; the
	// operator proxy-trim shed ranking and proxy audit DO read them (see
	// proxy_trim.go and proxy_audit.go), and neither writes them.
	Score float64 `json:"score,omitempty"`
	// Graded is true once a stage-1 table probe has recorded a DECIDABLE
	// result for this proxy. Distinct from Score: a decidable 0.0 is a
	// graded failure, while Score==0 with Graded=false means "never graded".
	Graded bool `json:"graded,omitempty"`
	// Failed lists the target hostnames that did not answer the last
	// stage-1 pass, for diagnostics and fleet reporting.
	Failed []string `json:"failed,omitempty"`
	// LastGraded is when the last stage-1 pass ran (decidable or not). The
	// grade sweep re-probes only entries older than the reaper stale
	// threshold (1-3h), so a DNS-gutted pass does not trigger a
	// re-probe-every-tick herd.
	LastGraded time.Time `json:"last_graded,omitempty"`
	// LastDecided is when the last DECIDABLE stage-1 pass landed, i.e. when
	// Score/Graded/Failed were last actually written. Unlike LastGraded it does
	// NOT move on a pass that completed without a verdict (stage-0 liveness
	// failed, or every host was unresolved or denied), so a consumer can tell a
	// new grade from an old one that merely had its attempt clock advanced.
	// Zero on entries graded before this field existed.
	LastDecided time.Time `json:"last_decided,omitempty"`

	// Pending is true when the last stage-1 pass REACHED the proxy but could
	// not produce a DECIDABLE verdict (fewer than half the intended sample
	// resolved through it — e.g. the box's DNS resolver could not answer most
	// health hosts, or the proxy is so strict/rate-limited that a
	// through-proxy answer could not be confirmed). This is the HONEST status
	// for a paid proxy we could not evaluate from this box, distinct from a
	// fabricated tier grade: it tells the operator "probe reached it but
	// cannot call it from here", NOT "it is graded F". Set true only on a
	// reachable-but-undecidable pass; cleared on any subsequent DECIDABLE
	// pass (which replaces the grade) and on a never-grade (never reached).
	// Never graded (no verdict, no reachability) leaves Graded/Pending both
	// false, meaning "not yet evaluated").
	Pending bool `json:"pending,omitempty"`
}

func proxyStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy.state"), nil
}

func readProxyState() (*ProxyState, error) {
	path, err := proxyStatePath()
	if err != nil {
		return nil, err
	}
	return readProxyStateFrom(path)
}

func readProxyStateFrom(path string) (*ProxyState, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &ProxyState{Proxies: map[string]ProxyEntry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read proxy.state: %w", err)
	}
	var s ProxyState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse proxy.state: %w", err)
	}
	if s.Proxies == nil {
		s.Proxies = map[string]ProxyEntry{}
	}
	return &s, nil
}

func writeProxyState(s *ProxyState) error {
	path, err := proxyStatePath()
	if err != nil {
		return err
	}
	return writeProxyStateTo(path, s)
}

func writeProxyStateTo(path string, s *ProxyState) error {
	// Every write moves the file forward to the current key format, even if
	// it was read as legacy (Version 0/1). adoptLegacyProxyState is what
	// actually migrates entries; this just records that a write in the
	// current format happened, so a reader downstream (or an operator
	// inspecting the file) knows the map keys are identity keys, not bare
	// addresses, from this point on. It is not a claim that every entry HAS
	// been migrated — a legacy entry with no current claimant is left alone
	// and can still be present after this bump (see adoptLegacyProxyState).
	s.Version = proxyStateVersion
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

// adoptLegacyProxyState migrates legacy (bare-address-keyed) entries in
// state.Proxies to the new identity-keyed format (ProxySettings.Key()) for
// every address the given desired settings actually claim right now,
// leaving every other entry untouched — already-migrated, or a legacy
// address with no current claimant (left alone for a later release to age
// out normally, per proxyStateVersion's doc comment).
//
// Idempotent and safe to call on every reload, not just "the first" one
// that sees a given address: an already-migrated address has no legacy
// entry left at its bare-address key, so a repeat call is a no-op for it.
//
// An address claimed by exactly one identity carries its legacy entry over
// intact — same ID, same health/grading history. An address claimed by two
// or more identities (the Decodo shared-gateway case: same host:port, two
// different accounts) adopts the legacy entry to the identity with the
// lexicographically smallest Key() — deterministic and stable across
// restarts, not "whichever the config happened to list first" — and every
// other identity at that address starts fresh via the normal
// resolveProxyID/tagProxySourceIfUnset path (new ID, zero history). This
// never fabricates history for an identity nothing could actually
// distinguish under the old model; it only ever preserves what already
// existed, and only for one winner.
//
// MUST run before any prune pass in the same reload (proxy_failure_history,
// proxy_auth_history, proxy_earnings_store all wipe an entry the moment
// they are handed a keep-set that no longer contains its old key) — see
// proxyStateVersion and the reload-engine phase that wires this in.
func adoptLegacyProxyState(state *ProxyState, desired []*connect.ProxySettings) (adopted, split int) {
	byAddress := make(map[string][]*connect.ProxySettings, len(desired))
	for _, s := range desired {
		if s == nil || s.Address == "" {
			continue
		}
		byAddress[s.Address] = append(byAddress[s.Address], s)
	}

	for address, settingsAtAddress := range byAddress {
		legacy, hasLegacy := state.Proxies[address]
		if !hasLegacy {
			continue
		}

		seen := map[string]bool{}
		var keys []string
		for _, s := range settingsAtAddress {
			k := s.Key()
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		winner := keys[0]

		if winner == address {
			// The winning (and, if len(keys)==1, only) identity carries no
			// distinguishing user: its Key() IS the bare address, so the
			// legacy entry is already correctly keyed. Nothing to adopt.
			continue
		}

		delete(state.Proxies, address)
		state.Proxies[winner] = legacy
		adopted++

		if len(keys) > 1 {
			split++
			tlog("[proxy][identity] %s split into %d identities on adoption; %s kept id=%d and history, the rest start fresh\n",
				address, len(keys), proxyKeyDisplay(winner), legacy.ID)
		}
	}
	return adopted, split
}

// proxyKeyDisplay renders a proxy identity key for operator-facing output
// ("addr" for no auth, "addr (user ab***yz)" for a shared-gateway
// identity), never the raw key — Key()'s \x1f separator must never reach a
// log line or terminal verbatim, and the password is never in the key to
// begin with (see ProxySettings.Key()).
func proxyKeyDisplay(key string) string {
	address, user := connect.SplitProxyKey(key)
	if user == "" {
		return address
	}
	return fmt.Sprintf("%s (user %s)", address, obfuscateUser(user))
}

// resolveProxyID returns the stable ID for an address.
// Known addresses keep their existing ID; new ones get the next counter value.
func resolveProxyID(state *ProxyState, address string) int {
	if entry, ok := state.Proxies[address]; ok {
		return entry.ID
	}
	id := nextProxyID()
	state.Proxies[address] = ProxyEntry{ID: id}
	return id
}

// tagProxySourceIfUnset records where a proxy address was first added from
// ("file", "internal", or "url"). Once set, the tag is never overwritten —
// an address keeps its original provenance across reloads and restarts, so
// source-scoped dead-proxy cleanup stays accurate even if the same address
// later also appears in a different source.
func tagProxySourceIfUnset(state *ProxyState, address, source string) {
	entry := state.Proxies[address]
	if entry.Source == "" {
		entry.Source = source
	}
	state.Proxies[address] = entry
}
