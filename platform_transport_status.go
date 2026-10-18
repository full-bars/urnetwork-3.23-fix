package connect

// Port note (v2026 engine -> 3.23 generation): this file is the fork's port of
// the upstream platform_transport_status.go, plus the minimal supporting
// surface its settings-only view needs in this generation. The upstream file
// reads a settings surface the 3.23 tree does not have yet (per-owner carrier
// budgets wired into the transport, the H1/H3 carrier byte budgets, and the
// extender memory policy). The supporting pieces below keep the upstream
// behavior for the status view and are narrowed where a dependency has no
// counterpart here:
//
//   - PlatformTransportSettings gains PlatformTransportBudget,
//     H1BudgetByteCount, H3BudgetByteCount, and ModePreferences (transport.go).
//     The pinned engine also admits the transport against them; the transport
//     swap that wires the reservations is a later slice, so nothing below the
//     status view reads them yet.
//   - H1MemoryReservationByteCount is the base row only. The pinned engine adds
//     the application graph's retained queue lanes and scratch minimums
//     (transport_h1_retained_budget.go); those arrive with the transport swap,
//     and the settings view's question -- "does one H1 claim fit the budget" --
//     is answered by the base row until then.
//   - h3NestedMemoryByteCount composes the DNS translation charge only. The
//     pinned engine also composes a configured outer extender's QUIC policy
//     (net_extender_memory.go); this generation has no extender memory policy.
//   - boundedQuicPacketTranslationSettings sizes the share both generations
//     have (the four payload packet queues). The pinned engine also sets
//     byte-denominated combine limits this generation's settings do not carry.
//   - defaultH3BudgetByteCount and its share helpers are ported verbatim; they
//     rest on MemoryBudget/MemoryTargetScaledByteCount, which exist here.
//   - DefaultPlatformTransportSettingsWithMemoryTarget is the settings
//     constructor the ported hierarchy test needs: it now returns a settings
//     value whose carrier budget and H1/H3 byte counts derive from one explicit
//     owner target. The H3 socket/window/datagram fields the pinned engine also
//     sizes here are not part of this generation's settings and stay out until
//     the transport swap.

// PlatformTransportAutoEligibility reports which configured Auto modes can be
// admitted by one otherwise-idle PlatformTransport under the structural byte
// and socket limits in settings. It deliberately ignores current budget usage:
// temporary candidate contention is not a stable platform capability and must
// not make settings UIs flicker between eligible and constrained.
//
// H3, H3-over-DNS, and H3-over-DNS-pump share one H3 reservation. When H1 is
// also configured, H1's reservation has precedence and every H3-family mode is
// eligible only if the budget can hold H1 and that shared H3 reservation
// together, including translation capacity when a DNS mode is configured.
// With no H1 in the Auto policy, H3 can use the budget by itself. This settings-
// only view cannot include an outer extender chosen by a caller's strategy.
func PlatformTransportAutoEligibility(
	settings *PlatformTransportSettings,
) map[TransportMode]bool {
	if settings == nil {
		settings = DefaultPlatformTransportSettings()
	}
	preferences := normalizeTransportModePreferences(settings.ModePreferences)
	eligible := map[TransportMode]bool{}
	if settings.PlatformTransportBudget == nil {
		for mode := range preferences {
			eligible[mode] = true
		}
		return eligible
	}

	budget := settings.PlatformTransportBudget.Stats()
	hasTransportSlot := budget.MaxTransportCount <= 0 || 1 <= budget.MaxTransportCount
	if !hasTransportSlot {
		return eligible
	}

	_, h1Configured := preferences[TransportModeH1]
	h1ByteCount := settings.H1MemoryReservationByteCount()
	h3ByteCount := settings.H3BudgetByteCount
	if h3ByteCount <= 0 {
		h3ByteCount = defaultH3BudgetByteCount()
	}
	h1Eligible := !h1Configured || h1ByteCount <= budget.TotalByteCount
	if h1Configured && h1Eligible {
		eligible[TransportModeH1] = true
	}

	transport := &PlatformTransport{
		settings:        settings,
		targetMode:      TransportModeAuto,
		modePreferences: preferences,
	}
	remainingByteCount := budget.TotalByteCount
	if h1Configured {
		// run waits for H1 admission before it starts the H3 mode group, so an
		// H1 claim that cannot fit prevents every Auto runner from starting.
		if !h1Eligible {
			return eligible
		}
		remainingByteCount -= h1ByteCount
	}
	// Subtract each admitted component instead of summing: a saturated h1
	// claim plus a positive h3 graph must not wrap or fit a max-sized root.
	if h3ByteCount < 0 || remainingByteCount < h3ByteCount {
		return eligible
	}
	remainingByteCount -= h3ByteCount
	nestedByteCount := transport.h3NestedMemoryByteCount()
	if nestedByteCount < 0 || remainingByteCount < nestedByteCount {
		return eligible
	}
	for mode := range preferences {
		if isH3TransportMode(mode) {
			eligible[mode] = true
		}
	}
	return eligible
}

// DefaultTransportModePreferences returns the production Auto policy. Lower
// numbers are preferred. H1 is deliberately the primary carrier while H3
// remains a direct fallback for paths where H1 is unavailable. Explicit H3
// selection bypasses this Auto ordering. DNS and DNS pump are progressively
// lower availability fallbacks.
func DefaultTransportModePreferences() map[TransportMode]int {
	return map[TransportMode]int{
		TransportModeH1: 1,
		TransportModeH3: 2,

		TransportModeH3Dns:     3,
		TransportModeH3DnsPump: 4,
	}
}

// normalizeTransportModePreferences drops unknown modes and non-positive
// priorities; an empty or nil policy selects the production default. It is the
// pinned engine's normalization, and it is the only place the settings-level
// Auto policy is interpreted in this generation.
func normalizeTransportModePreferences(preferences map[TransportMode]int) map[TransportMode]int {
	if preferences == nil {
		return DefaultTransportModePreferences()
	}
	normalized := map[TransportMode]int{}
	for mode, priority := range preferences {
		switch mode {
		case TransportModeH3, TransportModeH1, TransportModeH3Dns, TransportModeH3DnsPump:
			if 0 < priority {
				normalized[mode] = priority
			}
		}
	}
	if len(normalized) == 0 {
		return DefaultTransportModePreferences()
	}
	return normalized
}

// isH3TransportMode reports whether mode is carried by the shared H3
// reservation: H3 direct, H3 over DNS, or H3 over the DNS pump.
func isH3TransportMode(mode TransportMode) bool {
	switch mode {
	case TransportModeH3, TransportModeH3Dns, TransportModeH3DnsPump:
		return true
	default:
		return false
	}
}

// H1MemoryReservationByteCount is the retained-memory claim one H1 carrier
// composes before admission: the connection/reader/scratch base. See the port
// note for what the pinned engine adds on top.
func (self *PlatformTransportSettings) H1MemoryReservationByteCount() ByteCount {
	if self == nil {
		return 0
	}
	if 0 < self.H1BudgetByteCount {
		return self.H1BudgetByteCount
	}
	return MemoryScaledByteCount(kib(512), kib(256))
}

// h3BudgetShareByteCount is the carrier's draw on one memory target. Zero
// means there is no budget: that is the absence of the surface rather than a
// small share, and the callers below keep the constant default in that case.
const h3BudgetShareDivisor = 8

func h3BudgetShareByteCount(memoryTargetByteCount ByteCount) ByteCount {
	if memoryTargetByteCount <= 0 {
		return 0
	}
	return memoryTargetByteCount / h3BudgetShareDivisor
}

// h3BudgetByteCountForMemoryTarget is the base reservation against the
// aggregate transport budget: the draw, floored at the working minimum that
// lets one explicitly selected H3 carrier fit the smallest supported host.
func h3BudgetByteCountForMemoryTarget(memoryTargetByteCount ByteCount) ByteCount {
	share := h3BudgetShareByteCount(memoryTargetByteCount)
	if share <= 0 {
		return MemoryTargetScaledByteCount(memoryTargetByteCount, mib(8), mib(3))
	}
	return max(mib(3), share)
}

func defaultH3BudgetByteCount() ByteCount {
	return h3BudgetByteCountForMemoryTarget(MemoryBudget())
}

// h3NestedMemoryByteCount is the retained memory one H3 mode graph adds on top
// of the carrier's own draw. In this generation that is the DNS translation
// charge, composed only when the mode being started -- or the Auto policy the
// transport was constructed with -- actually includes a DNS mode. See the port
// note for the extender half the pinned engine also composes.
func (self *PlatformTransport) h3NestedMemoryByteCount() ByteCount {
	if self.targetMode != TransportModeH3Dns && self.targetMode != TransportModeH3DnsPump &&
		!(self.targetMode == TransportModeAuto &&
			(0 < self.modePreferences[TransportModeH3Dns] ||
				0 < self.modePreferences[TransportModeH3DnsPump])) {
		return 0
	}
	_, byteCount := boundedQuicPacketTranslationSettings()
	return byteCount
}

// boundedQuicPacketTranslationSettings is the translation memory policy's
// bounded settings and the retained charge they carry: the four payload queues
// (in/out/forward/readPipeline) with one producer-held packet each. See the
// port note for the byte-denominated combine limits the pinned engine sets on
// top. It is shared with the status view so a "does the H3 group fit" answer
// sizes the same DNS graph the translation path would retain.
func boundedQuicPacketTranslationSettings() (*PacketTranslationSettings, ByteCount) {
	settings := DefaultPacketTranslationSettings()
	settings.SequenceBufferSize = min(settings.SequenceBufferSize, 4)
	queueByteCount := ByteCount(4*(settings.SequenceBufferSize+1)) * kib(4)
	return settings, queueByteCount
}

// DefaultPlatformTransportSettingsWithMemoryTarget returns platform carrier
// settings whose admission limits derive from one explicit owner memory target,
// and which owns a private carrier budget. A nonpositive target retains the
// legacy process-global defaults. See the port note for the H3 socket, window,
// and datagram fields the pinned engine also sizes here.
func DefaultPlatformTransportSettingsWithMemoryTarget(
	memoryTargetByteCount ByteCount,
) *PlatformTransportSettings {
	settings := DefaultPlatformTransportSettings()
	if memoryTargetByteCount <= 0 {
		return settings
	}
	settings.PlatformTransportBudget =
		NewPlatformTransportBudgetForMemoryTarget(memoryTargetByteCount)
	settings.H1BudgetByteCount = MemoryTargetScaledByteCount(
		memoryTargetByteCount,
		kib(512),
		kib(256),
	)
	settings.H3BudgetByteCount = h3BudgetByteCountForMemoryTarget(memoryTargetByteCount)
	return settings
}
