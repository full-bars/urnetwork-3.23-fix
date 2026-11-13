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
