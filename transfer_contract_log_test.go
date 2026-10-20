package connect

import (
	"strings"
	"testing"
)

// The close and checkpoint lines are matched by their `[contract] closed` and
// `[contract] checkpointed` tokens: LOG_REFERENCE.md and the release notes both
// quote them, so a marker placed inside the message would break every parser
// matching them. The marker leads the line instead.
func TestCloseContractLineKeepsItsTokens(t *testing.T) {
	destination := NewId()

	closed := closeContractLine(false, 100, 200, 50, destination)
	if !strings.Contains(closed, "[contract] closed") {
		t.Fatalf("close token broken: %q", closed)
	}
	if !strings.HasPrefix(closed, "🚪 ") {
		t.Fatalf("marker must lead the line: %q", closed)
	}
	if !strings.Contains(closed, "acked=") || !strings.Contains(closed, "allotted=") {
		t.Fatalf("counters missing: %q", closed)
	}

	checkpointed := closeContractLine(true, 100, 200, 50, destination)
	if !strings.Contains(checkpointed, "[contract] checkpointed") {
		t.Fatalf("checkpoint token broken: %q", checkpointed)
	}
	if !strings.HasPrefix(checkpointed, "📍 ") {
		t.Fatalf("checkpoint marker must lead the line: %q", checkpointed)
	}
	// The two tokens must stay distinguishable to a matcher.
	if strings.Contains(checkpointed, "[contract] closed") {
		t.Fatalf("checkpoint line must not carry the close token: %q", checkpointed)
	}
}

func TestCloseContractLineWithProxy(t *testing.T) {
	destination := NewId()
	proxyDesc := "proxy[15304] (203.0.113.21:1081)"

	closed := closeContractLineWithProxy(false, 100, 200, 50, destination, proxyDesc)
	if !strings.Contains(closed, "[contract] closed proxy[15304] (203.0.113.21:1081)") {
		t.Fatalf("close line missing proxy: %q", closed)
	}
	if !strings.HasPrefix(closed, "🚪 ") {
		t.Fatalf("marker must lead the line: %q", closed)
	}

	checkpointed := closeContractLineWithProxy(true, 100, 200, 50, destination, proxyDesc)
	if !strings.Contains(checkpointed, "[contract] checkpointed proxy[15304] (203.0.113.21:1081)") {
		t.Fatalf("checkpoint line missing proxy: %q", checkpointed)
	}
	if !strings.HasPrefix(checkpointed, "📍 ") {
		t.Fatalf("checkpoint marker must lead the line: %q", checkpointed)
	}
}
