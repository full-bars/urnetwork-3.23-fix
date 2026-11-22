package urnettools

import (
	"errors"
	"testing"
	"time"
)

func TestTopLingerReadings(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ps := []Provider{{User: "alice", Unit: "a.service"}, {User: "alice", Unit: "b.service"}, {User: "bob", Unit: "c.service"}, {User: "skip"}}
	got := collectTopLinger(ps, now, func(p Provider) bool { return p.Unit != "" }, func(user string) (bool, error) {
		if user == "bob" {
			return false, errors.New("unavailable")
		}
		return false, nil
	})
	if len(got) != 1 || got[0].User != "alice" || got[0].Enabled || !got[0].CheckedAt.Equal(now) {
		t.Fatalf("unexpected readings: %+v", got)
	}
}
