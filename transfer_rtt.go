package connect

import (
	"fmt"
	"sync"
	"time"

	"container/heap"

	"github.com/urnetwork/connect/protocol"
)

type rttWindowItem struct {
	sendTime    time.Time
	receiveTime time.Time
	rtt         time.Duration

	heapIndex int
}

func newRttWindowItem(sendTime time.Time, receiveTime time.Time) *rttWindowItem {
	return &rttWindowItem{
		sendTime:    sendTime,
		receiveTime: receiveTime,
		rtt:         receiveTime.Sub(sendTime),
	}
}

type RttWindow struct {
	log           Logger
	windowTimeout time.Duration
	rttScale      float32
	minScaledRtt  time.Duration
	maxScaledRtt  time.Duration

	stateLock       sync.Mutex
	window          []*rttWindowItem
	windowTailIndex int
	windowHeadIndex int

	rtts *rttHeap
}

func NewRttWindow(
	log Logger,
	windowSize int,
	windowTimeout time.Duration,
	rttScale float32,
	minScaledRtt time.Duration,
	maxScaledRtt time.Duration,
) *RttWindow {
	if windowSize == 0 {
		panic(fmt.Errorf("Window size must non-zero: %d", windowSize))
	}
	window := make([]*rttWindowItem, windowSize)

	return &RttWindow{
		log:             loggerOrDefault(log),
		windowTimeout:   windowTimeout,
		rttScale:        rttScale,
		minScaledRtt:    minScaledRtt,
		maxScaledRtt:    maxScaledRtt,
		window:          window,
		windowTailIndex: 0,
		windowHeadIndex: 0,
		rtts:            newRttHeap(),
	}
}

// must be called inside the state lock
func (self *RttWindow) coalesce(windowTime time.Time) {
	windowStartTime := windowTime.Add(-self.windowTimeout)
	for self.windowTailIndex != self.windowHeadIndex {
		item := self.window[self.windowTailIndex]
		if !item.receiveTime.Before(windowStartTime) {
			break
		}
		self.rtts.Remove(item)
		self.window[self.windowTailIndex] = nil
		self.windowTailIndex = (self.windowTailIndex + 1) % len(self.window)
	}
}

// OpenTag starts an RTT sample: it returns a tag stamped with the current
// time that the caller attaches to a sent pack and gets threaded back in the
// peer's ack, then passes to CloseTag.
func (self *RttWindow) OpenTag() *protocol.Tag {
	return self.openTag(time.Now())
}

// openTag is OpenTag with an explicit send time, carried as unix
// milliseconds in the tag.
func (self *RttWindow) openTag(sendTime time.Time) *protocol.Tag {
	// sendTime
	return &protocol.Tag{
		SendTime: uint64(sendTime.UnixMilli()),
	}
}

// CloseTag completes an RTT sample opened by OpenTag: it records the current
// time as the receive time and adds the resulting round-trip sample to the
// window.
func (self *RttWindow) CloseTag(tag *protocol.Tag) {
	self.closeTag(tag, time.Now())
}

// closeTag is CloseTag with an explicit receive time. A tag whose send time
// is after the receive time is ignored.
func (self *RttWindow) closeTag(tag *protocol.Tag, receiveTime time.Time) {
	sendTime := time.UnixMilli(int64(tag.SendTime))
	if receiveTime.Before(sendTime) {
		// ignore
		return
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.coalesce(receiveTime)

	item := newRttWindowItem(
		sendTime,
		receiveTime,
	)
	self.rtts.Add(item)

	if replaceItem := self.window[self.windowHeadIndex]; replaceItem != nil {
		self.rtts.Remove(replaceItem)
	}
	self.window[self.windowHeadIndex] = item
	self.windowHeadIndex = (self.windowHeadIndex + 1) % len(self.window)
	if self.windowTailIndex == self.windowHeadIndex {
		self.windowTailIndex = (self.windowTailIndex + 1) % len(self.window)
	}
}

// min(max of window * scale, overall max)
func (self *RttWindow) ScaledRtt() time.Duration {
	return self.scaledRtt(time.Now())
}

// MeanRtt returns the mean RTT of the samples currently in the window,
// after dropping samples older than the window timeout.
func (self *RttWindow) MeanRtt() time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.coalesce(time.Now())
	return self.rtts.MeanRtt()
}

// scaledRtt computes the base resend delay: the mean RTT of the current
// window, scaled by the window's rttScale factor, rounded to whole
// milliseconds, and clamped to [minScaledRtt, maxScaledRtt].
func (self *RttWindow) scaledRtt(sendTime time.Time) time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.coalesce(sendTime)

	useRtt := self.rtts.MeanRtt()
	scaledRtt := min(
		max(
			time.Duration(float32(useRtt/time.Millisecond)*self.rttScale)*time.Millisecond,
			self.minScaledRtt,
		),
		self.maxScaledRtt,
	)
	self.log.V(2).Infof("[rtt]scaled=%dms\n", scaledRtt/time.Millisecond)
	return scaledRtt
}

type rttHeap struct {
	items  []*rttWindowItem
	netRtt time.Duration
}

// `heap` is a min heap
func newRttHeap() *rttHeap {
	h := &rttHeap{
		items:  []*rttWindowItem{},
		netRtt: time.Duration(0),
	}
	heap.Init(h)
	return h
}

// Add pushes a sample onto the min-heap and adds its RTT to the running
// total that MeanRtt divides by. The heap has no internal lock; the owning
// window's state lock guards all access.
func (self *rttHeap) Add(item *rttWindowItem) {
	heap.Push(self, item)
	self.netRtt += item.rtt
}

// Remove removes a sample from the min-heap by its maintained heap index
// and subtracts its RTT from the running total.
func (self *rttHeap) Remove(item *rttWindowItem) {
	heap.Remove(self, item.heapIndex)
	self.netRtt -= item.rtt
}

// MinRtt returns the smallest sample RTT in the heap (its top), or zero
// when the heap is empty.
func (self *rttHeap) MinRtt() time.Duration {
	n := len(self.items)
	if n == 0 {
		return time.Duration(0)
	}
	return self.items[0].rtt
}

// MeanRtt returns the mean sample RTT over all items in the heap, or zero
// when the heap is empty.
func (self *rttHeap) MeanRtt() time.Duration {
	n := len(self.items)
	if n == 0 {
		return 0
	}
	return self.netRtt / time.Duration(n)
}

// `heap.Interface`

// Len returns the number of samples in the heap.
func (self *rttHeap) Len() int {
	return len(self.items)
}

// Less orders the min-heap by raw sample RTT: the smallest RTT sorts
// first. The ordering is by RTT, not by any scaled value.
func (self *rttHeap) Less(i, j int) bool {
	return self.items[i].rtt < self.items[j].rtt
}

// Swap exchanges two heap entries, keeping each item's heap index in sync.
func (self *rttHeap) Swap(i, j int) {
	a := self.items[i]
	b := self.items[j]
	b.heapIndex = i
	self.items[i] = b
	a.heapIndex = j
	self.items[j] = a
}

// Push appends a sample to the heap and records its index.
func (self *rttHeap) Push(x any) {
	item := x.(*rttWindowItem)
	item.heapIndex = len(self.items)
	self.items = append(self.items, item)
}

// Pop removes and returns the last heap entry.
func (self *rttHeap) Pop() any {
	n := len(self.items)
	item := self.items[n-1]
	self.items[n-1] = nil
	self.items = self.items[0 : n-1]
	return item
}
