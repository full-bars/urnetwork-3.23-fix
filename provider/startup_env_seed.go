package main

import (
	"encoding/json"
	"os"
)

// seedEnvFromControlState makes URNETWORK_PROFILE and URNETWORK_RAMLOGS
// reflect the provider's own persisted/queued control state, before
// initGlog() (called from this package's init(), i.e. before main() even
// starts) reads them via plain os.Getenv. Those two settings — unlike every
// other control-socket key — are consumed that early (initGlog decides
// whether to redirect stdout/stderr to a ramlog; RunStartupAudit and
// applyTurboSettings read them too, later but still via the same env var),
// so a normal loadControlState()+mergePendingOverrides() call inside
// provide() runs far too late to affect them: by the time provide() is
// even reached, initGlog() has already made its one-shot, unrepeatable
// decision from whatever was in the environment.
//
// This intentionally does NOT call the real mergePendingOverrides() (which
// persists and deletes pending_overrides.json) — that stays exactly where
// it is, inside provide(), as the single owner of that file's lifecycle.
// This is a read-only peek at the same two sources (provider_state.json,
// pending_overrides.json) purely to compute what the env vars should be
// before initGlog() runs; provide()'s later call still does the real
// merge-and-persist-and-delete once the process is actually providing.
//
// Best-effort throughout: any error here (missing HOME, malformed file,
// unreadable file) just means the env vars are left as whatever the
// caller/systemd unit already set, exactly as if this function didn't
// exist — never blocks or fails startup.
func seedEnvFromControlState() {
	values := map[string]string{}

	if data, err := os.ReadFile(mustControlStatePath()); err == nil {
		// Decode into a temporary map first: json.Unmarshal can populate
		// fields decoded before a later UnmarshalTypeError, so unmarshaling
		// straight into values could seed URNETWORK_PROFILE from a
		// partially-decoded, otherwise-invalid provider_state.json.
		var onDisk map[string]string
		if json.Unmarshal(data, &onDisk) == nil {
			values = onDisk
		}
	}

	// Overlay anything urnet-tools queued while the provider wasn't
	// running — this is the common "set profile before first start" case
	// PR X exists for. Applied on top of the persisted snapshot, in order,
	// same precedence as the real mergePendingOverrides().
	if data, err := os.ReadFile(mustPendingOverridesPath()); err == nil {
		var ops []pendingOp
		if json.Unmarshal(data, &ops) == nil {
			for _, op := range ops {
				switch op.Op {
				case "set":
					values[op.Key] = op.Value
				case "clear":
					delete(values, op.Key)
				}
			}
		}
	}

	if profile, ok := values["profile"]; ok && profile != "" {
		os.Setenv("URNETWORK_PROFILE", profile)
	}
	if ramlogs, ok := values["ramlogs"]; ok {
		if ramlogs == "on" {
			os.Setenv("URNETWORK_RAMLOGS", "1")
		} else {
			os.Setenv("URNETWORK_RAMLOGS", "0")
		}
	}
}

// mustControlStatePath returns controlStatePath(), or "" on error (e.g. no
// resolvable HOME) — os.ReadFile("") fails harmlessly, matching this file's
// best-effort contract without needing every caller to handle the error.
func mustControlStatePath() string {
	path, err := controlStatePath()
	if err != nil {
		return ""
	}
	return path
}

// mustPendingOverridesPath is mustControlStatePath's counterpart for
// pending_overrides.json.
func mustPendingOverridesPath() string {
	path, err := pendingOverridesPath()
	if err != nil {
		return ""
	}
	return path
}
