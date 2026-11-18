package urnettools

import "testing"

type overviewPanicSource struct {
	snap          *NodeSnapshot
	snapshotPanic bool
	extenderPanic bool
}

func (s overviewPanicSource) Fetch(Provider) (*NodeSnapshot, error) {
	if s.snapshotPanic {
		panic("snapshot")
	}
	return s.snap, nil
}
func (s overviewPanicSource) FetchExtender(Provider) ExtenderReading {
	if s.extenderPanic {
		panic("extender")
	}
	return ExtenderReading{Source: SourceControlSocket}
}

func TestTopOverviewPanicIsolation(t *testing.T) {
	snap := &NodeSnapshot{}
	got := topFetchOverview(overviewPanicSource{snap: snap, extenderPanic: true}, Provider{}, 4, 1)
	if got.snap != snap || got.err != nil || got.extender.Source != SourceUnknown {
		t.Fatalf("extender panic: %+v", got)
	}
	got = topFetchOverview(overviewPanicSource{snapshotPanic: true}, Provider{}, 4, 1)
	if got.snap != nil || got.err == nil {
		t.Fatalf("snapshot panic: %+v", got)
	}
	got = topFetchOverview(overviewPanicSource{snap: snap}, Provider{}, 4, 1)
	if got.snap != snap || got.err != nil || got.extender.Source != SourceControlSocket {
		t.Fatalf("normal: %+v", got)
	}
}
