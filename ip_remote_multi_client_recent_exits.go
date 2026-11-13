package connect

import (
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
