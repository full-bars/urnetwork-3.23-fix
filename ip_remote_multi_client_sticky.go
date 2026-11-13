package connect

// ip_remote_multi_client_sticky.go - the redial of the user's Fixed IP (a
// "sticky" window): window min = max = 1 is a fixed profile of one exit.
//
// A sticky window that loses its exit is meant to ask the platform for that
// same provider first (enumerateStickyRedial), so a provider that is still
// online comes back with the same egress ip.
//
// This fork has no transport-loss removal that could fill the memory, so
// nothing calls stickyRedial.Remember yet and the enumerator finds nothing to
// redial. The read side is in place for that producer.
//
// Threading: the pending re-dial has its own small lock and never takes the
// window lock, so it may be used from the resize pass, the enumerator and
// SetPerformanceProfile alike.

import (
	"sync"
)

// An optional generator capability: look up one provider by client id.
type MultiClientGeneratorWithClientId interface {
	NextDestinationsForClientId(clientId Id, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error)
}

// Reports whether a performance profile is the user's Fixed IP: a fixed window
// type sized to exactly one exit.
func stickyExitProfile(performanceProfile *PerformanceProfile) bool {
	_, windowSize, ok := performanceProfile.FixedWindow()
	return ok && windowSize.FixedWindowSize == 1
}

// Reports whether this window holds the user's Fixed IP.
func (self *multiClientWindow) stickyExit() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return stickyExitProfile(self.performanceProfile)
}

// The exit a sticky window lost, held for the next discovery round. One
// pending exit at most. The zero value is ready.
type stickyRedial struct {
	stateLock   sync.Mutex
	destination MultiHopId
	stats       DestinationStats
	pending     bool
}

// Holds a lost exit for the next discovery round, replacing any exit already
// held.
func (self *stickyRedial) Remember(destination MultiHopId, stats DestinationStats) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.destination = destination
	self.stats = stats
	self.pending = true
}

// Hands the held exit to one discovery round and clears it, so an exit that
// does not come back costs one platform round trip, not a retry loop.
func (self *stickyRedial) Take() (destination MultiHopId, stats DestinationStats, ok bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.pending {
		return
	}
	destination, stats, ok = self.destination, self.stats, true
	self.destination = MultiHopId{}
	self.stats = DestinationStats{}
	self.pending = false
	return
}

// Drops the held exit: the user changed the profile or asked for new exits.
func (self *stickyRedial) Forget() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.destination = MultiHopId{}
	self.stats = DestinationStats{}
	self.pending = false
}

// One by-id lookup for this window: in selector mode with the window's
// `select` when the generator has it, else as before. Must be called with no
// lock held.
func (self *multiClientWindow) lookupClientId(
	clientIdGenerator MultiClientGeneratorWithClientId,
	clientId Id,
	excludeDestinations []MultiHopId,
	rankMode string,
) (capabilityDiscovery, error) {
	if generator, ok := self.generator.(interface {
		NextDestinationsForClientIdWithCapabilities(
			clientId Id,
			excludeDestinations []MultiHopId,
			rankMode string,
			providerSelect FindProvidersSelect,
		) (map[MultiHopId]DestinationStats, bool, error)
	}); ok {
		destinations, selectorMode, err := generator.NextDestinationsForClientIdWithCapabilities(
			clientId,
			excludeDestinations,
			rankMode,
			self.providerSelect(),
		)
		return capabilityDiscovery{
			destinations: destinations,
			selectorMode: selectorMode,
		}, err
	}
	destinations, err := clientIdGenerator.NextDestinationsForClientId(clientId, excludeDestinations, rankMode)
	return capabilityDiscovery{
		destinations: destinations,
	}, err
}

// The stats a remembered exit is dialed again with: as it was discovered, with
// the labels of a fresh selector-mode answer. Any other answer keeps the
// remembered labels.
func relabelledStats(remembered DestinationStats, fresh DestinationStats, selectorMode bool) DestinationStats {
	if !selectorMode {
		return remembered
	}
	remembered.IpFamily = fresh.IpFamily
	remembered.Capabilities = fresh.Capabilities
	return remembered
}

// Runs ahead of a discovery round: when the window holds a lost exit it asks
// the generator for that provider by client id, and ok reports a round of that
// one destination. An answer without it falls back to discovery in the same
// round. A platform that cannot be reached keeps the exit pending for the
// enumerator's retry.
func (self *multiClientWindow) enumerateStickyRedial(
	excludeDestinations []MultiHopId,
	rankMode string,
) (redial enumeratedDestination, ok bool, err error) {
	destination, stats, pending := self.stickyRedial.Take()
	if !pending || !self.stickyExit() {
		return
	}
	if _, fixed := self.generator.FixedDestinationSize(); fixed {
		// a fixed destination set already names its providers
		return
	}
	clientIdGenerator, capable := self.generator.(MultiClientGeneratorWithClientId)
	if !capable {
		return
	}
	answer, err := windowGeneratorCall(
		self.ctx,
		self.settings.WindowGeneratorTimeout,
		func() (capabilityDiscovery, error) {
			return self.lookupClientId(
				clientIdGenerator,
				destination.Tail(),
				excludeDestinations,
				rankMode,
			)
		},
		nil,
	)
	if err != nil {
		self.stickyRedial.Remember(destination, stats)
		return enumeratedDestination{}, false, err
	}
	for candidate, candidateStats := range answer.destinations {
		if candidate.Tail() == destination.Tail() {
			loggerOrDefault(self.log).Infof("%s\n", relEvent(
				"sticky_redial",
				"provider", destination.Tail(),
				"action", "dial",
			))
			return enumeratedDestination{
				destination:  destination,
				stats:        relabelledStats(stats, candidateStats, answer.selectorMode),
				stickyRedial: true,
			}, true, nil
		}
	}
	loggerOrDefault(self.log).Infof("%s\n", relEvent(
		"sticky_redial",
		"provider", destination.Tail(),
		"action", "unavailable",
	))
	return enumeratedDestination{}, false, nil
}
