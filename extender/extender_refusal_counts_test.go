package extender

import "testing"

func TestRefusalCountsBounded(t *testing.T) {
	var server ExtenderServer
	for _, stage := range extenderRefusalStageNames {
		server.usage.recordRefusal(stage)
	}
	server.usage.recordRefusal("future stage")
	counts := server.RefusalCounts()
	for _, stage := range extenderRefusalStageNames {
		want := uint64(1)
		if stage == "other" {
			want = 2
		}
		if counts[stage] != want {
			t.Errorf("%s = %d, want %d", stage, counts[stage], want)
		}
	}
	if got := testing.AllocsPerRun(1000, func() { server.usage.recordRefusal("service") }); got != 0 {
		t.Fatalf("recordRefusal allocated %v times", got)
	}
}
