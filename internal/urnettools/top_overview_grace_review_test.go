package urnettools

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

func TestTopOverviewRetainsSnapshotDuringTimeoutGrace(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	snap := topFlowing(t)
	m.apply(m.gen, snap, nil)
	m.apply(m.gen, nil, context.DeadlineExceeded)
	if m.providerSnaps[0] != snap {
		t.Fatal("timeout within grace discarded overview snapshot")
	}
	clock.Advance(topLossGrace)
	m.apply(m.gen, nil, context.DeadlineExceeded)
	if m.providerSnaps[0] != nil {
		t.Fatal("snapshot retained after grace expired")
	}
}
