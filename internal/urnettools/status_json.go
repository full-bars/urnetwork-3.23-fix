package urnettools

import (
	"encoding/json"
	"fmt"
	"time"
)

// statusJSONSchema identifies the stable JSON shape so scripts can detect
// changes without parsing field names. Bump the major version only when a
// field is removed or renamed; adding fields is backward compatible.
const statusJSONSchema = "urnet-tools-status/1"

type statusProviderJSON struct {
	User      string `json:"user"`
	Unit      string `json:"unit"`
	Binary    string `json:"binary"`
	Version   string `json:"version"`
	StateDir  string `json:"state_dir"`
	PID       int    `json:"pid"`
	Running   bool   `json:"running"`
	Network   string `json:"network"`
	NetworkID string `json:"network_id"`
	// JWTExpires is a pointer so a provider with no token omits the field
	// instead of printing the zero time.
	JWTExpires    *time.Time `json:"jwt_expires,omitempty"`
	ControlSocket bool       `json:"control_socket"`
}

type statusUpdateJSON struct {
	Installed   string `json:"installed"`
	Latest      string `json:"latest"`
	Available   bool   `json:"available"`
	Unavailable bool   `json:"unavailable"`
}

// statusDoc is the stable document printed by `status --json-summary`.
// Rows carry the same names and levels rendered on the summary screen.
type statusDoc struct {
	Schema   string             `json:"schema"`
	Provider statusProviderJSON `json:"provider"`
	Rows     []statusRow        `json:"rows"`
	Update   statusUpdateJSON   `json:"update"`
	ExitCode int                `json:"exit_code"`
}

// buildStatusDoc assembles the --json document from the live data.
func buildStatusDoc(p Provider, d statusData) statusDoc {
	available := false
	if !d.Update.Unavailable && d.Update.Tag != "" && p.Version != "" {
		available = compareVersionTags(p.Version, d.Update.Tag) < 0
	}
	up := statusUpdateJSON{
		Installed:   p.Version,
		Latest:      d.Update.Tag,
		Available:   available,
		Unavailable: d.Update.Unavailable,
	}
	sock := controlSocketReachable(p)
	var jwtExpires *time.Time
	if !p.JWTExpires.IsZero() {
		expires := p.JWTExpires
		jwtExpires = &expires
	}
	return statusDoc{
		Schema: statusJSONSchema,
		Provider: statusProviderJSON{
			User:          p.User,
			Unit:          p.Unit,
			Binary:        p.Binary,
			Version:       p.Version,
			StateDir:      p.StateDir,
			PID:           p.PID,
			Running:       p.Running,
			Network:       p.netLabel(),
			NetworkID:     p.NetworkID,
			JWTExpires:    jwtExpires,
			ControlSocket: sock,
		},
		Rows:     d.Rows,
		Update:   up,
		ExitCode: d.ExitCode,
	}
}

// printStatusJSON prints the stable status document for
// `status --json-summary` (schema urnet-tools-status/1). `status --json` keeps
// printing the raw snapshot.
func printStatusJSON(p Provider) error {
	d := collectStatusData(p)
	doc := buildStatusDoc(p, d)
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
