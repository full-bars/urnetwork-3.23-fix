package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/connect"
)

// extender_status.go is the one place that answers "is the extender working?".
// The health line, the control socket's `extender` command, and through it
// `urnet-tools extender status`, `status` and `top` all read the same
// extenderControlReading, so they can never disagree.

// What one address family is doing, in a word.
const (
	// the platform listed this host on this family
	extenderFamilyActivated = "activated"
	// the platform answered and said no
	extenderFamilyRefused = "refused"
	// the request failed (network, no public address, tcp not answering...)
	extenderFamilyFailed = "failed"
	// activation cannot be tried because tcp 443 is not bound
	extenderFamilyHeld = "held"
	// nothing tried yet: the direct slot has not signed in, or the first try is due
	extenderFamilyWaiting = "waiting"
)

// extenderPortReading is one carrier port: tcp 443, udp 443 (quic), dns 4053.
type extenderPortReading struct {
	Name  string `json:"name"`
	Up    bool   `json:"up"`
	Error string `json:"error,omitempty"`
}

// extenderFamilyReading is one address family's activation state.
type extenderFamilyReading struct {
	IpVersion int    `json:"ip_version"`
	State     string `json:"state"`
	Ip        string `json:"ip,omitempty"`
	// why it is not active, in the platform's or the system's words
	Detail            string `json:"detail,omitempty"`
	LastAttemptUnixMs int64  `json:"last_attempt_unix_ms,omitempty"`
	NextAttemptUnixMs int64  `json:"next_attempt_unix_ms,omitempty"`
	ExpireUnixMs      int64  `json:"expire_unix_ms,omitempty"`
}

// extenderControlReading is the whole extender picture, and the JSON the
// control socket returns for `extender`.
type extenderControlReading struct {
	// Known is false for a provider too old to answer; urnet-tools reads it to
	// tell "off" from "this build has no extender status"
	Known         bool   `json:"known"`
	Setting       string `json:"setting"` // on or off, what the operator asked for
	SettingSource string `json:"setting_source,omitempty"`
	Running       bool   `json:"running"`
	RoleState     string `json:"role_state,omitempty"`
	OffReason     string `json:"off_reason,omitempty"`
	Note          string `json:"note,omitempty"`
	// Activating is whether the direct slot has attached the activation loop
	Activating bool                    `json:"activating"`
	Ports      []extenderPortReading   `json:"ports,omitempty"`
	DnsPorts   []int                   `json:"dns_ports,omitempty"`
	TcpRetries int64                   `json:"tcp_retries,omitempty"`
	Families   []extenderFamilyReading `json:"families,omitempty"`
	// LiveClients is the connections open over every carrier right now
	LiveClients int `json:"live_clients"`
	// Served is requests served for clients (see extender_tally.go)
	Served          extenderTallyWindows `json:"served"`
	RelayBytesIn    int64                `json:"relay_bytes_in,omitempty"`
	RelayBytesOut   int64                `json:"relay_bytes_out,omitempty"`
	RefusalsByStage map[string]uint64    `json:"refusals_by_stage,omitempty"`
}

func unixMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// Reading gathers the current picture. Cheap enough for a status call and the
// health tick: it reads in-memory state and one small file at most.
func (m *extenderManager) Reading() extenderControlReading {
	on, source := extenderSetting(m.environ)
	reading := extenderControlReading{Known: true, Setting: "off", SettingSource: source}
	if on {
		reading.Setting = "on"
	}
	role := m.Role()
	if role == nil {
		reading.RoleState = extenderStateOff
		reading.OffReason = m.OffReason()
		reading.Served = m.tallyWindows()
		return reading
	}
	m.sampleTally(role)
	snapshot := role.snapshot()
	reading.Running = true
	reading.RoleState = snapshot.State
	reading.Note = snapshot.Note
	reading.TcpRetries = snapshot.TcpRetries
	reading.DnsPorts = snapshot.DnsPorts
	reading.LiveClients = snapshot.Connections
	reading.RelayBytesIn = snapshot.Stats.IngressByteCount
	reading.RelayBytesOut = snapshot.Stats.EgressByteCount
	if len(snapshot.RefusalCounts) != 0 {
		reading.RefusalsByStage = snapshot.RefusalCounts
	}
	reading.Served = m.tallyWindows()

	portUp := func(carrier string) (bool, string) {
		for _, up := range snapshot.Carriers {
			if up == carrier {
				return true, ""
			}
		}
		return false, snapshot.ListenErrors[carrier]
	}
	for _, port := range []struct{ name, carrier string }{
		{fmt.Sprintf("tcp %d", connect.ExtenderTcpPort), connect.ExtenderCarrierTcp},
		{fmt.Sprintf("udp %d", connect.ExtenderTcpPort), connect.ExtenderCarrierQuic},
		{fmt.Sprintf("dns %d", connect.ExtenderDnsPort), connect.ExtenderCarrierDns},
	} {
		up, listenError := portUp(port.carrier)
		reading.Ports = append(reading.Ports, extenderPortReading{Name: port.name, Up: up, Error: listenError})
	}
	tcpUp, _ := portUp(connect.ExtenderCarrierTcp)

	activator := role.currentActivator()
	reading.Activating = activator != nil
	var families map[int]*connect.ExtenderFamilyActivationStatus
	if activator != nil {
		families = map[int]*connect.ExtenderFamilyActivationStatus{}
		for _, family := range activator.Status().Families {
			families[family.IpVersion] = family
		}
	}
	for _, ipVersion := range []int{4, 6} {
		reading.Families = append(reading.Families, extenderFamilyReadingOf(ipVersion, families[ipVersion], activator != nil, tcpUp))
	}
	return reading
}

func extenderFamilyReadingOf(ipVersion int, family *connect.ExtenderFamilyActivationStatus, attached bool, tcpUp bool) extenderFamilyReading {
	reading := extenderFamilyReading{IpVersion: ipVersion, State: extenderFamilyWaiting}
	if !attached {
		reading.Detail = "waiting for the direct slot to sign in"
		return reading
	}
	if family != nil {
		reading.LastAttemptUnixMs = unixMs(family.LastAttemptTime)
		reading.NextAttemptUnixMs = unixMs(family.NextAttemptTime)
		reading.ExpireUnixMs = unixMs(family.ExpireTime)
	}
	switch {
	case family != nil && family.Activated:
		reading.State = extenderFamilyActivated
		if family.Ip.IsValid() {
			reading.Ip = family.Ip.String()
		}
	case !tcpUp:
		reading.State = extenderFamilyHeld
		reading.Detail = "tcp 443 is not bound, so the platform cannot verify this host"
	case family != nil && family.LastRefused:
		reading.State = extenderFamilyRefused
		reading.Detail = family.LastError
	case family != nil && family.LastError != "":
		reading.State = extenderFamilyFailed
		reading.Detail = family.LastError
	}
	return reading
}

// formatExtenderHealthLine is the single line written with the periodic
// [health] lines.
func formatExtenderHealthLine(reading extenderControlReading, now time.Time) string {
	const tag = "❤️ [health][extender]"
	if !reading.Known {
		return tag + " unavailable"
	}
	if !reading.Running {
		reason := reading.OffReason
		if reason == "" {
			reason = "not running"
		}
		return fmt.Sprintf("%s OFF: %s | served 24h %d, 7d %d, lifetime %d requests",
			tag, reason, reading.Served.Last24h, reading.Served.Last7d, reading.Served.Lifetime)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", tag, strings.ReplaceAll(reading.RoleState, "_", " "))

	var ports []string
	tcpDown := false
	for _, port := range reading.Ports {
		switch {
		case port.Up:
			ports = append(ports, port.Name+" up")
		case port.Error != "":
			ports = append(ports, fmt.Sprintf("%s DOWN (%s)", port.Name, port.Error))
		default:
			ports = append(ports, port.Name+" DOWN")
		}
		if strings.HasPrefix(port.Name, "tcp") && !port.Up {
			tcpDown = true
		}
	}
	if len(ports) != 0 {
		b.WriteString(" | " + strings.Join(ports, ", "))
	}
	if tcpDown {
		b.WriteString(" | tcp 443 is down, so this host is NOT listed and gets no clients")
	}

	for _, family := range reading.Families {
		name := extenderFamilyLabel(family.IpVersion)
		b.WriteString(" | ")
		switch family.State {
		case extenderFamilyActivated:
			fmt.Fprintf(&b, "%s ACTIVE", name)
			if family.Ip != "" {
				b.WriteString(" " + family.Ip)
			}
		case extenderFamilyHeld:
			fmt.Fprintf(&b, "%s not active (held until tcp 443 is bound)", name)
		case extenderFamilyWaiting:
			fmt.Fprintf(&b, "%s waiting", name)
			if family.Detail != "" {
				b.WriteString(" (" + family.Detail + ")")
			}
			b.WriteString(nextTryText(family, now))
		default:
			fmt.Fprintf(&b, "%s not active (%s)", name, family.State)
			if family.Detail != "" {
				b.WriteString(": " + family.Detail)
			}
			b.WriteString(nextTryText(family, now))
		}
	}

	fmt.Fprintf(&b, " | clients now %d, requests served 24h %d, 7d %d, lifetime %d",
		reading.LiveClients, reading.Served.Last24h, reading.Served.Last7d, reading.Served.Lifetime)
	return b.String()
}

func nextTryText(family extenderFamilyReading, now time.Time) string {
	if family.NextAttemptUnixMs == 0 {
		return ""
	}
	next := time.UnixMilli(family.NextAttemptUnixMs)
	if !next.After(now) {
		return ", next try due now"
	}
	return ", next try " + clockLabel(next)
}
