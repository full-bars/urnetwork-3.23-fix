package main

import "strings"

// Direct-only H3 defaults.
//
// A direct-mode node has exactly ONE identity — the native [direct] transport —
// so the resource argument that keeps H3 off by default on a pooled node (one
// QUIC/UDP socket set per proxy identity) does not apply. H3 runs BESIDE H1 and
// falls back to H1 quietly, so enabling it on a direct-only box costs one extra
// socket for a second, independently-fallback transport. On a pooled node that
// cost multiplies by the identity count, which is why the default stays off
// there.
//
// Trigger: proxy resolution has SETTLED as direct-only
// (proxyResolutionZeroValid). Deliberately not "zero proxies right now": a node
// whose proxies are still loading would otherwise flip H3 on and then off again
// as the pool arrives, and the flapping is worse than either steady state.
//
// Precedence: these are DEFAULTS only. resolveH3 and friends consult the
// control keys first, so an explicit `urnet-tools set h3 off` (or a persisted
// pending override) always wins and the automatic default never overrides a
// deliberate operator choice.
const (
	// autoEnableH3 is the implicit opt-in applied to a direct-only node.
	autoEnableH3 = true
)

// directOnlyH3Defaults returns the default transport opt-in state for the h3
// control keys on this node: all on when the node has settled as direct-only
// with the direct transport available, otherwise off (today's behavior).
//
// proxyResolutionNoSource means direct-only AND direct is switched off — nothing
// is served at all — so there is no identity for H3 to attach to and the
// defaults stay off.
func directOnlyH3Defaults(resolution int32) bool {
	return autoEnableH3 && resolution == proxyResolutionZeroValid
}

// defaultH3Setting is the startup fallback for the "h3" key when the operator
// has not set it: on for a settled direct-only node, off otherwise.
func defaultH3Setting(state *controlState, resolution int32) bool {
	if directOnlyH3Defaults(resolution) {
		return true
	}
	return h3Enabled()
}

// resolveH3Setting mirrors resolveH3 but consults the direct-only default
// instead of only URNETWORK_H3, so the runtime gate can turn H3 on for a
// direct-only node without the operator setting anything.
//
// An explicit control key still wins.
func resolveH3Setting(state *controlState, resolution int32) bool {
	if state != nil {
		if v, ok := state.get("h3"); ok {
			return strings.EqualFold(v, "on")
		}
	}
	return defaultH3Setting(state, resolution)
}

// directOnlyDatagramDefault reports whether the H3 DATAGRAM offer/send
// experiments default on for a direct-only node. Same reasoning as H3 itself:
// one identity, one extra socket, quiet H1 fallback. Off for pooled nodes.
func directOnlyDatagramDefault(resolution int32) bool {
	return autoEnableH3 && resolution == proxyResolutionZeroValid
}

// applyDirectOnlyH3Defaults reapplies the H3 transport defaults when a node
// enters or leaves direct-only resolution, unless the operator set those keys.
//
// WHY THIS EXISTS SEPARATELY FROM THE STARTUP PATH. The startup control replay
// runs before the first proxy reload, so at that moment the resolution state is
// still Pending and a direct-only default cannot be known yet. This is called
// from the reload that settles the state, which is the first point at which
// "direct-only" is a fact.
//
// Precedence is unchanged: an explicit control key (persisted "off" included)
// always wins. Only unset keys are updated using the current resolution.
func applyDirectOnlyH3Defaults() {
	resolution := currentProxyResolution()
	state := globalControlState
	if state == nil {
		return
	}
	for _, key := range []string{"h3", "h3_datagram", "h3_datagram_send"} {
		if v, set := state.get(key); set && strings.TrimSpace(v) != "" {
			// Operator set it (either value): leave it alone.
			continue
		}
		value := onOff(directOnlyDatagramDefault(resolution))
		if key == "h3" {
			value = onOff(defaultH3Setting(state, resolution))
		}
		if err := applyLiveSideEffect(key, value); err != nil {
			tlog("[t]h3 default: failed to apply %s=%s: %s\n", key, value, err)
			continue
		}
		tlog("[t]h3 default: applied %s=%s\n", key, value)
	}
}
