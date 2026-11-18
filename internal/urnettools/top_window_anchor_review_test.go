package urnettools

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

func TestTopWindowSeriesAnchorTracksAbsoluteBins(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1800000120, 0)}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.snap = topFlowing(t)
	for _, tc := range []struct{ hours, seconds, capacity int }{{1, 60, 60}, {24, 900, 96}} {
		m.windowHours = tc.hours
		s := m.series()
		want := clock.Now().Unix() / int64(tc.seconds)
		if s.anchor != want || s.capacity != tc.capacity {
			t.Fatalf("%dh anchor=%d capacity=%d, want %d and %d", tc.hours, s.anchor, s.capacity, want, tc.capacity)
		}
		clock.Advance(time.Duration(tc.seconds) * time.Second)
		if got := m.series().anchor; got != want+1 {
			t.Fatalf("%dh next anchor=%d, want %d", tc.hours, got, want+1)
		}
	}
}
