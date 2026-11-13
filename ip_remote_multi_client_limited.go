package connect

import "math"

// windowSizing is the normal window's last resize result before reserves.
type windowSizing struct {
	published  bool
	active     bool
	targetSize int
	hardMax    int
}

type limitedSizing struct {
	enabled          bool
	fraction         float64
	sizeMax          int
	targetSize       int
	hardMax          int
	hardMaxUnbounded bool
}

type limitedWindowHooks struct {
	sizing func() limitedSizing
	wake   func()
}

func limitedWindowFractionEnabled(fraction float64) bool {
	return 0 < fraction && !math.IsInf(fraction, 0)
}

func limitedWindowShare(fraction float64, count int) int {
	share := math.Floor(fraction * float64(count))
	if math.IsNaN(share) || share < 1 {
		return 1
	}
	return int(min(share, float64(math.MaxInt32)))
}

func limitedWindowSize(sizing limitedSizing) WindowSizeSettings {
	windowSize := WindowSizeSettings{WindowSizeReconnectScale: 1}
	if !sizing.enabled || !limitedWindowFractionEnabled(sizing.fraction) || sizing.targetSize <= 0 {
		return windowSize
	}
	targetSize := limitedWindowShare(sizing.fraction, sizing.targetSize)
	hardMax := 0
	if !sizing.hardMaxUnbounded && 0 < sizing.hardMax {
		hardMax = limitedWindowShare(sizing.fraction, sizing.hardMax)
	}
	if 0 < sizing.sizeMax {
		targetSize = min(targetSize, sizing.sizeMax)
		if hardMax <= 0 {
			hardMax = sizing.sizeMax
		} else {
			hardMax = min(hardMax, sizing.sizeMax)
		}
	}
	if 0 < hardMax {
		targetSize = min(targetSize, hardMax)
	}
	windowSize.WindowSizeMin = targetSize
	windowSize.WindowSizeMax = targetSize
	windowSize.WindowSizeHardMax = hardMax
	return windowSize
}

// publishSizing takes no window lock on entry. Wake outside the lock so the
// limited window can safely read all sibling snapshots.
func (self *multiClientWindow) publishSizing(sizing windowSizing) {
	self.stateLock.Lock()
	changed := self.publishedSizing != sizing
	self.publishedSizing = sizing
	self.stateLock.Unlock()
	if changed {
		if hooks := self.limitedHooks.Load(); hooks != nil {
			hooks.wake()
		}
	}
}

func (self *multiClientWindow) publishedSizingSnapshot() windowSizing {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.publishedSizing
}

// limitedWindowSizing reads normal-window snapshots without holding two locks.
func (self *RemoteUserNatMultiClient) limitedWindowSizing() limitedSizing {
	sizing := limitedSizing{fraction: self.LimitedWindowSizeFraction(), sizeMax: self.settings.LimitedWindowSizeMax}
	generator, capable := self.generator.(MultiClientGeneratorWithCapabilities)
	if !capable {
		return sizing
	}
	if mode, known := generator.SelectorMode(); known && !mode {
		return sizing
	}
	sizing.enabled = true
	for windowType, window := range self.windows {
		if windowType == WindowTypeLimited {
			continue
		}
		published := window.publishedSizingSnapshot()
		if !published.published {
			sizing.targetSize = 0
			return sizing
		}
		if !published.active {
			continue
		}
		sizing.targetSize = max(sizing.targetSize, published.targetSize)
		if published.hardMax <= 0 {
			sizing.hardMaxUnbounded = true
		} else {
			sizing.hardMax = max(sizing.hardMax, published.hardMax)
		}
	}
	return sizing
}

func (self *multiClientWindow) limitedWindowSize() WindowSizeSettings {
	hooks := self.limitedHooks.Load()
	if hooks == nil || self.stickyExit() {
		return limitedWindowSize(limitedSizing{})
	}
	return limitedWindowSize(hooks.sizing())
}

// raceField collects compatible candidates from the first usable normal window
// and the limited window. The normal candidates keep priority at the race cap.
func (self *RemoteUserNatMultiClient) raceField(windowTypes []WindowType, need flowNeed) []*multiClientChannel {
	var clients []*multiClientChannel
	for _, windowType := range windowTypes {
		if window := self.windows[windowType]; window != nil && windowType != WindowTypeLimited {
			clients = candidatesForFlow(window.OrderedClients(), need)
			if len(clients) > 0 {
				break
			}
		}
	}
	if window := self.windows[WindowTypeLimited]; window != nil {
		clients = append(clients, candidatesForFlow(window.OrderedClients(), need)...)
	}
	return limitedClientsLast(clients)
}

// limitedClientsLast keeps normal exits ahead when the race is bounded.
func limitedClientsLast(clients []*multiClientChannel) []*multiClientChannel {
	ordered := make([]*multiClientChannel, 0, len(clients))
	for _, client := range clients {
		if client.windowType() != WindowTypeLimited {
			ordered = append(ordered, client)
		}
	}
	if len(ordered) == len(clients) || len(ordered) == 0 {
		return clients
	}
	for _, client := range clients {
		if client.windowType() == WindowTypeLimited {
			ordered = append(ordered, client)
		}
	}
	return ordered
}

// LimitedWindowSize returns the number of live limited exits, including warned exits.
func (self *RemoteUserNatMultiClient) LimitedWindowSize() int {
	window := self.windows[WindowTypeLimited]
	if window == nil {
		return 0
	}
	count := 0
	for _, client := range window.unorderedClients() {
		if !client.IsDone() {
			count++
		}
	}
	return count
}

// LimitedWindowTargetSize returns the target from the last resize pass.
func (self *RemoteUserNatMultiClient) LimitedWindowTargetSize() int {
	window := self.windows[WindowTypeLimited]
	if window == nil {
		return 0
	}
	window.stateLock.Lock()
	defer window.stateLock.Unlock()
	return max(0, window.requestTargetCount)
}

// LimitedWindowSizeFraction returns the configured or overridden fraction.
func (self *RemoteUserNatMultiClient) LimitedWindowSizeFraction() float64 {
	return math.Float64frombits(self.limitedWindowSizeFractionBits.Load())
}

// SetLimitedWindowSizeFraction updates the fraction and wakes the window.
func (self *RemoteUserNatMultiClient) SetLimitedWindowSizeFraction(fraction float64) {
	previous := math.Float64frombits(self.limitedWindowSizeFractionBits.Swap(math.Float64bits(fraction)))
	window := self.windows[WindowTypeLimited]
	if window == nil {
		return
	}
	if limitedWindowFractionEnabled(previous) && !limitedWindowFractionEnabled(fraction) {
		window.shuffle()
		return
	}
	window.resizeMonitor.NotifyAll()
}
