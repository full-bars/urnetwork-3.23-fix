package connect

import (
	"errors"
	"slices"
	"sync"
	"time"
)

// recentExit describes a destination lost during a local outage.
type recentExit struct {
	destination MultiHopId
	stats       DestinationStats
	lostTime    time.Time
	reason      string
}

func recentExitValid(exit recentExit, now time.Time, timeout time.Duration) bool {
	return timeout > 0 && now.Sub(exit.lostTime) < timeout
}

// recentExitMemory bounds remembered exits and invalidates in-flight restores
// whenever the user forgets an exit. Its zero value is ready for use.
type recentExitMemory struct {
	stateLock  sync.Mutex
	exits      []recentExit
	maxCount   int
	generation uint64
}

func (self *recentExitMemory) Remember(exit recentExit, maxCount int, timeout time.Duration) (evicted []recentExit, expired []recentExit) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.maxCount = maxCount
	self.exits = slices.DeleteFunc(self.exits, func(held recentExit) bool {
		if held.destination == exit.destination {
			return true
		}
		if !recentExitValid(held, exit.lostTime, timeout) {
			expired = append(expired, held)
			return true
		}
		return false
	})
	self.exits = append(self.exits, exit)
	self.sortWithLock()
	return self.evictWithLock(), expired
}

func (self *recentExitMemory) Take(now time.Time, timeout time.Duration) (taken []recentExit, expired []recentExit, generation uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, exit := range self.exits {
		if recentExitValid(exit, now, timeout) {
			taken = append(taken, exit)
		} else {
			expired = append(expired, exit)
		}
	}
	self.exits = nil
	return taken, expired, self.generation
}

func (self *recentExitMemory) Restore(exits []recentExit, generation uint64) (evicted []recentExit) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(exits) == 0 || generation != self.generation {
		return nil
	}
	for _, exit := range exits {
		if !slices.ContainsFunc(self.exits, func(held recentExit) bool { return held.destination == exit.destination }) {
			self.exits = append(self.exits, exit)
		}
	}
	self.sortWithLock()
	return self.evictWithLock()
}

func (self *recentExitMemory) Current(generation uint64) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return generation == self.generation
}

func (self *recentExitMemory) Forget() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := len(self.exits)
	self.exits = nil
	self.generation++
	return count
}

func (self *recentExitMemory) ForgetProvider(clientId Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := len(self.exits)
	self.exits = slices.DeleteFunc(self.exits, func(held recentExit) bool { return held.destination.Tail() == clientId })
	self.generation++
	return len(self.exits) < count
}

func (self *recentExitMemory) sortWithLock() {
	slices.SortStableFunc(self.exits, func(a, b recentExit) int { return a.lostTime.Compare(b.lostTime) })
}

func (self *recentExitMemory) evictWithLock() (evicted []recentExit) {
	if n := len(self.exits) - max(0, self.maxCount); n > 0 {
		evicted = slices.Clone(self.exits[:n])
		self.exits = slices.Delete(self.exits, 0, n)
	}
	return evicted
}

func recentExitMemoryMaxCount(windowSize WindowSizeSettings) int {
	if windowSize.WindowSizeHardMax > 0 {
		return windowSize.WindowSizeHardMax
	}
	return max(windowSize.WindowSizeMin, windowSize.WindowSizeMax, windowSize.FixedWindowSize)
}

func (self *multiClientWindow) recentExitMemoryTimeout() time.Duration {
	if self.settings == nil {
		return 0
	}
	return self.settings.RecentExitMemoryTimeout
}

func (self *multiClientWindow) recentExitGenerator() (MultiClientGeneratorWithClientId, bool) {
	if self.generator == nil {
		return nil, false
	}
	if _, fixed := self.generator.FixedDestinationSize(); fixed {
		return nil, false
	}
	generator, ok := self.generator.(MultiClientGeneratorWithClientId)
	return generator, ok
}

func (self *multiClientWindow) enumerateRecentExits(exclude []MultiHopId, rankMode string, discover func([]MultiHopId) ([]enumeratedDestination, error)) ([]enumeratedDestination, error) {
	discoverTracked := func(exclude []MultiHopId) ([]enumeratedDestination, error) {
		results, err := discover(exclude)
		self.recentDiscoveryFailed.Store(err != nil)
		return results, err
	}
	timeout := self.recentExitMemoryTimeout()
	generator, capable := self.recentExitGenerator()
	if timeout <= 0 || !capable || self.stickyExit() {
		return discoverTracked(exclude)
	}
	exits, generation := self.takeRecentExits(time.Now(), timeout)
	if len(exits) == 0 {
		return discoverTracked(exclude)
	}
	discoverExclude := slices.Clone(exclude)
	for _, exit := range exits {
		discoverExclude = append(discoverExclude, exit.destination)
	}
	if self.recentDiscoveryFailed.Load() {
		discovered, err := discoverTracked(discoverExclude)
		if err != nil {
			self.restoreRecentExits(exits, generation)
			return nil, err
		}
		recovered := self.resolveRecentExits(self.lookupRecentExits(generator, exits, exclude, rankMode)(), generation)
		return appendEnumeratedDestinations(recovered, discovered), nil
	}
	wait := self.lookupRecentExits(generator, exits, exclude, rankMode)
	discovered, err := discoverTracked(discoverExclude)
	recovered := self.resolveRecentExits(wait(), generation)
	if err != nil {
		if len(recovered) == 0 {
			return nil, err
		}
		return recovered, nil
	}
	return appendEnumeratedDestinations(recovered, discovered), nil
}

func (self *multiClientWindow) takeRecentExits(now time.Time, timeout time.Duration) ([]recentExit, uint64) {
	taken, expired, generation := self.recentExits.Take(now, timeout)
	for _, exit := range expired {
		loggerOrDefault(self.log).Infof("%s\n", relEvent("recent_exit", "provider", exit.destination.Tail(), "action", "expire", "age", now.Sub(exit.lostTime)))
	}
	present := map[MultiHopId]bool{}
	for _, client := range self.unorderedClients() {
		if client.args != nil {
			present[client.Destination()] = true
		}
	}
	exits := make([]recentExit, 0, len(taken))
	for _, exit := range taken {
		if !present[exit.destination] {
			exits = append(exits, exit)
		}
	}
	return exits, generation
}

type recentExitAnswer struct {
	exit  recentExit
	found bool
	err   error
	stats DestinationStats
}

var errRecentExitLookupIncomplete = errors.New("recent exit lookup incomplete")

func (self *multiClientWindow) lookupRecentExits(generator MultiClientGeneratorWithClientId, exits []recentExit, exclude []MultiHopId, rankMode string) func() []recentExitAnswer {
	answers := make([]recentExitAnswer, len(exits))
	var lookups sync.WaitGroup
	for i, exit := range exits {
		answers[i] = recentExitAnswer{exit: exit, err: errRecentExitLookupIncomplete}
		lookups.Add(1)
		go HandleError(func() {
			defer lookups.Done()
			answer, err := windowGeneratorCall(self.ctx, self.settings.WindowGeneratorTimeout, func() (capabilityDiscovery, error) {
				return self.lookupClientId(generator, exit.destination.Tail(), exclude, rankMode)
			}, nil)
			stats := exit.stats
			found := false
			for candidate, fresh := range answer.destinations {
				if candidate.Tail() == exit.destination.Tail() {
					stats = relabelledStats(stats, fresh, answer.selectorMode)
					found = self.admitsCapabilities(stats.Capabilities)
					break
				}
			}
			answers[i] = recentExitAnswer{exit: exit, found: err == nil && found, err: err, stats: stats}
		})
	}
	return func() []recentExitAnswer { lookups.Wait(); return answers }
}

func (self *multiClientWindow) resolveRecentExits(answers []recentExitAnswer, generation uint64) []enumeratedDestination {
	if !self.recentExits.Current(generation) {
		return nil
	}
	var recovered []enumeratedDestination
	var unanswered []recentExit
	for _, answer := range answers {
		switch {
		case answer.err != nil:
			unanswered = append(unanswered, answer.exit)
		case answer.found:
			loggerOrDefault(self.log).Infof("%s\n", relEvent("recent_exit", "provider", answer.exit.destination.Tail(), "action", "try", "reason", answer.exit.reason, "age", time.Since(answer.exit.lostTime)))
			recovered = append(recovered, enumeratedDestination{destination: answer.exit.destination, stats: answer.stats, recentExit: true})
		default:
			loggerOrDefault(self.log).Infof("%s\n", relEvent("recent_exit", "provider", answer.exit.destination.Tail(), "action", "drop", "reason", "unavailable"))
		}
	}
	self.restoreRecentExits(unanswered, generation)
	return recovered
}

func (self *multiClientWindow) restoreRecentExits(exits []recentExit, generation uint64) {
	for _, exit := range self.recentExits.Restore(exits, generation) {
		loggerOrDefault(self.log).Infof("%s\n", relEvent("recent_exit", "provider", exit.destination.Tail(), "action", "drop", "reason", "cap"))
	}
}

func appendEnumeratedDestinations(leading, rest []enumeratedDestination) []enumeratedDestination {
	ordered := make([]enumeratedDestination, 0, len(leading)+len(rest))
	seen := map[MultiHopId]bool{}
	for _, destinations := range [][]enumeratedDestination{leading, rest} {
		for _, destination := range destinations {
			if !seen[destination.destination] {
				seen[destination.destination] = true
				ordered = append(ordered, destination)
			}
		}
	}
	return ordered
}
