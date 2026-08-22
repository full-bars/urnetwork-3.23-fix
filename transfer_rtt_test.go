package connect

import (
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
)

func TestRttWindow(t *testing.T) {
	rttWindow := NewRttWindow(nil, 4, 1*time.Second, 1.0, 0, time.Second)

	assert.Equal(t, rttWindow.ScaledRtt(), time.Duration(0))

	start := time.Now()

	tag1 := rttWindow.openTag(start)
	tag2 := rttWindow.openTag(start.Add(50 * time.Millisecond))
	tag3 := rttWindow.openTag(start.Add(100 * time.Millisecond))
	tag4 := rttWindow.openTag(start.Add(150 * time.Millisecond))

	assert.Equal(t, rttWindow.scaledRtt(start.Add(150*time.Millisecond)), time.Duration(0))

	rttWindow.closeTag(tag2, start.Add(300*time.Millisecond)) // 250

	assert.Equal(t, rttWindow.ScaledRtt(), 250*time.Millisecond)

	rttWindow.closeTag(tag4, start.Add(300*time.Millisecond)) // 150
	rttWindow.closeTag(tag3, start.Add(500*time.Millisecond)) // 400
	rttWindow.closeTag(tag1, start.Add(800*time.Millisecond)) // 800

	assert.Equal(t, rttWindow.scaledRtt(start.Add(800*time.Millisecond)), (250+150+400+800)/4*time.Millisecond)

	start2 := start.Add(2 * time.Second)
	tag21 := rttWindow.openTag(start2)
	tag22 := rttWindow.openTag(start2)
	tag23 := rttWindow.openTag(start2)
	tag24 := rttWindow.openTag(start2)
	tag25 := rttWindow.openTag(start2)

	// clears the window
	rttWindow.closeTag(tag21, start2.Add(500*time.Millisecond))

	assert.Equal(t, rttWindow.scaledRtt(start2.Add(500*time.Millisecond)), 500*time.Millisecond)

	rttWindow.closeTag(tag22, start2.Add(500*time.Millisecond))

	assert.Equal(t, rttWindow.scaledRtt(start2.Add(500*time.Millisecond)), 500*time.Millisecond)

	rttWindow.closeTag(tag23, start2.Add(500*time.Millisecond))
	rttWindow.closeTag(tag24, start2.Add(500*time.Millisecond))

	// cycle window
	rttWindow.closeTag(tag25, start2.Add(100*time.Millisecond))

	assert.Equal(t, rttWindow.scaledRtt(start2.Add(100*time.Millisecond)), (500+500+500+100)/4*time.Millisecond)
}

func TestRttWindowMeanRtt(t *testing.T) {
	rttWindow := NewRttWindow(nil, 8, 5*time.Second, 1.0, 0, time.Second)

	assert.Equal(t, rttWindow.MeanRtt(), time.Duration(0))

	start := time.Now()

	// add three samples with known RTTs
	tag1 := rttWindow.openTag(start)
	tag2 := rttWindow.openTag(start.Add(100 * time.Millisecond))
	tag3 := rttWindow.openTag(start.Add(200 * time.Millisecond))

	rttWindow.closeTag(tag1, start.Add(150*time.Millisecond)) // 150ms
	rttWindow.closeTag(tag2, start.Add(350*time.Millisecond)) // 250ms
	rttWindow.closeTag(tag3, start.Add(700*time.Millisecond)) // 500ms

	// expected mean should be approximately 300ms (150+250+500)/3
	expectedMean := (150*time.Millisecond + 250*time.Millisecond + 500*time.Millisecond) / 3
	delta := rttWindow.MeanRtt() - expectedMean
	if delta < 0 {
		delta = -delta
	}
	if delta > 5*time.Millisecond {
		t.Errorf("MeanRtt = %v, expected ~%v (diff %v)", rttWindow.MeanRtt(), expectedMean, delta)
	}
}

func TestComputeFillFraction(t *testing.T) {
	fallback := float32(0.7)

	// zero RTT falls back to settings value
	assert.Equal(t, computeFillFraction(0, fallback), fallback)

	// at or below 100ms → high (0.85)
	assert.Equal(t, computeFillFraction(50*time.Millisecond, fallback), float32(0.85))
	assert.Equal(t, computeFillFraction(100*time.Millisecond, fallback), float32(0.85))

	// above 1000ms → low (0.7)
	assert.Equal(t, computeFillFraction(1000*time.Millisecond, fallback), float32(0.7))
	assert.Equal(t, computeFillFraction(2000*time.Millisecond, fallback), float32(0.7))

	// midpoint of range → should be approximately 0.775 (halfway between 0.85 and 0.7 at 550ms)
	midFill := computeFillFraction(550*time.Millisecond, fallback)
	if midFill < 0.774 || midFill > 0.776 {
		t.Errorf("expected mid fill ~0.775, got %f", midFill)
	}

	// increasing RTT decreases fill
	fillA := computeFillFraction(200*time.Millisecond, fallback)
	fillB := computeFillFraction(800*time.Millisecond, fallback)
	if fillA <= fillB {
		t.Errorf("expected fill at 200ms (%f) > fill at 800ms (%f)", fillA, fillB)
	}
}
