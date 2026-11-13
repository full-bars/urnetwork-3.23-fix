package connect

import (
	"math"
	"testing"
)

func TestLimitedWindowFractionWithoutWindow(t *testing.T) {
	client := &RemoteUserNatMultiClient{}
	client.SetLimitedWindowSizeFraction(.25)
	if client.LimitedWindowSizeFraction() != .25 || client.LimitedWindowSize() != 0 || client.LimitedWindowTargetSize() != 0 {
		t.Fatal("fraction or absent window state incorrect")
	}
	client.SetLimitedWindowSizeFraction(math.NaN())
	if !math.IsNaN(client.LimitedWindowSizeFraction()) {
		t.Fatal("NaN fraction was not preserved")
	}
}

func TestLimitedWindowSizingWithoutCapabilityGenerator(t *testing.T) {
	client := &RemoteUserNatMultiClient{settings: &MultiClientSettings{LimitedWindowSizeMax: 3}}
	client.SetLimitedWindowSizeFraction(.5)
	got := client.limitedWindowSizing()
	if got.enabled || got.fraction != .5 || got.sizeMax != 3 {
		t.Fatalf("unexpected sizing: %+v", got)
	}
}

func TestLimitedWindowSize(t *testing.T) {
	tests := []struct {
		name         string
		sizing       limitedSizing
		target, hard int
	}{
		{"disabled", limitedSizing{fraction: .5, targetSize: 8}, 0, 0},
		{"zero fraction", limitedSizing{enabled: true, targetSize: 8}, 0, 0},
		{"nan", limitedSizing{enabled: true, fraction: math.NaN(), targetSize: 8}, 0, 0},
		{"infinity", limitedSizing{enabled: true, fraction: math.Inf(1), targetSize: 8}, 0, 0},
		{"no normal target", limitedSizing{enabled: true, fraction: .5}, 0, 0},
		{"minimum one", limitedSizing{enabled: true, fraction: .1, targetSize: 1, hardMax: 2}, 1, 1},
		{"bounded", limitedSizing{enabled: true, fraction: .5, targetSize: 8, hardMax: 12}, 4, 6},
		{"hard cap", limitedSizing{enabled: true, fraction: .5, targetSize: 8, hardMax: 4}, 2, 2},
		{"size cap", limitedSizing{enabled: true, fraction: .5, targetSize: 8, hardMax: 12, sizeMax: 3}, 3, 3},
		{"unbounded capped", limitedSizing{enabled: true, fraction: .5, targetSize: 8, hardMaxUnbounded: true, sizeMax: 3}, 3, 3},
		{"unbounded", limitedSizing{enabled: true, fraction: .5, targetSize: 8, hardMax: 12, hardMaxUnbounded: true}, 4, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := limitedWindowSize(tt.sizing)
			if got.WindowSizeMin != tt.target || got.WindowSizeMax != tt.target || got.WindowSizeHardMax != tt.hard || got.WindowSizeReconnectScale != 1 {
				t.Fatalf("unexpected size: %+v", got)
			}
		})
	}
}

func TestPublishWindowSizingWakesOnlyOnChange(t *testing.T) {
	window := &multiClientWindow{}
	wakes := 0
	window.limitedHooks.Store(&limitedWindowHooks{wake: func() {
		wakes++
		// Snapshot here verifies the wake does not hold the window lock.
		if !window.publishedSizingSnapshot().published {
			t.Error("snapshot not published")
		}
	}})
	value := windowSizing{published: true, active: true, targetSize: 4, hardMax: 8}
	window.publishSizing(value)
	window.publishSizing(value)
	value.targetSize = 5
	window.publishSizing(value)
	if wakes != 2 || window.publishedSizingSnapshot() != value {
		t.Fatalf("wakes=%d snapshot=%+v", wakes, window.publishedSizingSnapshot())
	}
}
