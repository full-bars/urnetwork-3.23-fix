package main

import (
	"sort"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// proxyAuditStatus is the proxy audit engine's last completed tick, published for
// the control socket ("audit", "status", read by `urnet-tools proxy audit status`)
// and for /metrics. It is a snapshot by value: readers never see a half-updated one.
type proxyAuditStatus struct {
	// Acting is true when proxy audit executes parks; false means it only
	// observes (proxy audit off, or hot restart off).
	Acting bool `json:"acting"`
	// NotActingReason names why it is only observing (auditReason values),
	// so the operator is told which switch to flip. Empty while acting.
	NotActingReason string `json:"not_acting_reason,omitempty"`
	// Parked lists the proxies the audit engine is holding out, with when each
	// backoff ends.
	Parked []proxyAuditParkedStatus `json:"parked,omitempty"`
	// WouldPark is how many proxies would be parked right now. Only meaningful
	// while observing; when acting they are parked instead.
	WouldPark int `json:"would_park"`
	// Distrusted and Thin report why the last pass parked nothing: the
	// correlated-failure breaker tripped, or too few proxies could be graded.
	Distrusted bool `json:"distrusted,omitempty"`
	Thin       bool `json:"thin_pass,omitempty"`
	// Parks24h is how many parks count against the rolling 24h budget.
	Parks24h int `json:"parks_24h"`
	// Paused is true while the paid proxy list is unreadable or empty and the
	// audit engine therefore does nothing; PausedSince is when that began. It is
	// usually a skipped tick (a file caught mid-edit) but a missing or unreadable
	// file keeps it paused until fixed, so it must be visible.
	Paused      bool      `json:"paused,omitempty"`
	PausedSince time.Time `json:"paused_since,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type proxyAuditParkedStatus struct {
	Addr  string    `json:"addr"`
	User  string    `json:"user,omitempty"` // obfuscated; tells accounts at one gateway apart
	Until time.Time `json:"until"`
}

var currentProxyAuditStatus atomic.Pointer[proxyAuditStatus]

// proxyAuditStatusSnapshot returns the last published status, or nil before the
// audit engine has completed a tick.
func proxyAuditStatusSnapshot() *proxyAuditStatus {
	return currentProxyAuditStatus.Load()
}

// publish stores a status snapshot for this completed tick.
func (g *proxyAuditor) publish(now time.Time, act bool, res proxyAuditResult) {
	st := &proxyAuditStatus{
		Acting:     act,
		Distrusted: res.Distrusted,
		Thin:       res.Thin,
		Parks24h:   len(g.st.parkTimes),
		UpdatedAt:  now,
	}
	if !g.pausedSince.IsZero() {
		st.Paused, st.PausedSince = true, g.pausedSince
	}
	if !act {
		st.WouldPark = len(res.Park)
		if g.env.notActingReason != nil {
			st.NotActingReason = g.env.notActingReason()
		}
	}
	// Parks are keyed by proxy identity; publish the address (what `proxy audit
	// release` takes) plus an obfuscated user, never the raw key.
	for key, rec := range g.st.parks {
		addr, user := connect.SplitProxyKey(key)
		p := proxyAuditParkedStatus{Addr: addr, Until: rec.until}
		if user != "" {
			p.User = obfuscateUser(user)
		}
		st.Parked = append(st.Parked, p)
	}
	sort.Slice(st.Parked, func(i, j int) bool {
		if st.Parked[i].Addr != st.Parked[j].Addr {
			return st.Parked[i].Addr < st.Parked[j].Addr
		}
		return st.Parked[i].User < st.Parked[j].User
	})
	currentProxyAuditStatus.Store(st)
	setProxyAuditSystemdState(len(st.Parked), st.Paused)
}
