package connect

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func TestRecentEnumerationRecoveryAndExclusion(t *testing.T) {
	id := RequireMultiHopId(NewId())
	other := RequireMultiHopId(NewId())
	generator := &stickyTestGenerator{answer: map[MultiHopId]DestinationStats{id: {}}}
	window := stickyTestWindow(generator, 2)
	window.recentExits.Remember(recentExit{destination: id, lostTime: time.Now()}, 2, time.Minute)
	result, err := window.enumerateRecentExits(nil, "", func(exclude []MultiHopId) ([]enumeratedDestination, error) {
		if !slices.Contains(exclude, id) {
			t.Error("remembered destination not excluded from discovery")
		}
		return []enumeratedDestination{{destination: other}, {destination: id}}, nil
	})
	if err != nil || len(result) != 2 || result[0].destination != id || !result[0].recentExit || result[1].destination != other {
		t.Fatalf("unexpected recovery: %+v, %v", result, err)
	}
	if len(generator.asked) != 1 || generator.asked[0] != id.Tail() {
		t.Fatal("expected lookup by client id")
	}
}

func TestRecentEnumerationFailures(t *testing.T) {
	id := RequireMultiHopId(NewId())
	errDown := errors.New("down")
	t.Run("unanswered lookup restored", func(t *testing.T) {
		generator := &stickyTestGenerator{err: errDown}
		window := stickyTestWindow(generator, 2)
		window.recentExits.Remember(recentExit{destination: id, lostTime: time.Now()}, 1, time.Minute)
		_, err := window.enumerateRecentExits(nil, "", func([]MultiHopId) ([]enumeratedDestination, error) { return nil, nil })
		taken, _, _ := window.recentExits.Take(time.Now(), time.Minute)
		if err != nil || len(taken) != 1 {
			t.Fatalf("lookup failure must restore: %v %v", taken, err)
		}
	})
	t.Run("discovery failure uses recovered", func(t *testing.T) {
		generator := &stickyTestGenerator{answer: map[MultiHopId]DestinationStats{id: {}}}
		window := stickyTestWindow(generator, 2)
		window.recentExits.Remember(recentExit{destination: id, lostTime: time.Now()}, 1, time.Minute)
		got, err := window.enumerateRecentExits(nil, "", func([]MultiHopId) ([]enumeratedDestination, error) { return nil, errDown })
		if err != nil || len(got) != 1 || got[0].destination != id {
			t.Fatalf("recovered exit lost: %v %v", got, err)
		}
	})
	t.Run("failed platform avoids lookup and restores", func(t *testing.T) {
		generator := &stickyTestGenerator{}
		window := stickyTestWindow(generator, 2)
		window.recentDiscoveryFailed.Store(true)
		window.recentExits.Remember(recentExit{destination: id, lostTime: time.Now()}, 1, time.Minute)
		_, err := window.enumerateRecentExits(nil, "", func([]MultiHopId) ([]enumeratedDestination, error) { return nil, errDown })
		taken, _, _ := window.recentExits.Take(time.Now(), time.Minute)
		if !errors.Is(err, errDown) || len(taken) != 1 || len(generator.asked) != 0 {
			t.Fatalf("failure amplification or lost entry: %v %v %v", err, taken, generator.asked)
		}
	})
}

func TestRecentEnumerationForgetInvalidatesAnswers(t *testing.T) {
	id := RequireMultiHopId(NewId())
	window := stickyTestWindow(&stickyTestGenerator{}, 2)
	window.recentExits.Remember(recentExit{destination: id, lostTime: time.Now()}, 1, time.Minute)
	exits, generation := window.takeRecentExits(time.Now(), time.Minute)
	window.recentExits.ForgetProvider(id.Tail())
	got := window.resolveRecentExits([]recentExitAnswer{{exit: exits[0], found: true}}, generation)
	if len(got) != 0 {
		t.Fatal("forgotten in-flight exit was offered")
	}
}
