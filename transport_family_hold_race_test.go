package connect

import (
	"sync"
	"testing"
)

// Concurrent updates of the hold inputs must leave the published gate equal to
// what the final inputs derive. Before updateHold took the lock, an older call
// could store a stale gate after a newer one and leave a connecting family held.
func TestUpdateHoldSettlesToTheFinalState(t *testing.T) {
	for round := 0; round < 200; round++ {
		transport := newPinnedRouteTestTransport()
		var wg sync.WaitGroup
		for worker := 0; worker < 8; worker++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				for i := 0; i < 50; i++ {
					if (worker+i)%2 == 0 {
						transport.familyHold.Store(int32(PlatformTransportStateSleeping))
					} else {
						transport.familyHold.Store(int32(PlatformTransportStateConnecting))
					}
					transport.updateHold()
				}
			}(worker)
		}
		wg.Wait()
		want := !transport.enabled.Load() || transport.familyHold.Load() != int32(PlatformTransportStateConnecting)
		if got, _ := transport.held.Get(); got != want {
			t.Fatalf("round %d: gate = %v, want %v for the final inputs", round, got, want)
		}
	}
}
