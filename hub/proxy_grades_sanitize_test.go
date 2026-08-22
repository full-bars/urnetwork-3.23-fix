package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Final-review CRITICAL regression: the node drawer serves the raw
// in-memory report (n.Proxies), so the persist-side tier allowlist alone
// left the drawer path exposed to a crafted tier. handleReport must
// sanitize grades at ingest, before the report enters the store.

func TestSanitizeProxyGrades_NullsInvalidTier(t *testing.T) {
	proxies := []proxyReport{
		{ID: "p1", Address: "1.2.3.4:1080", Tier: `"><img src=x onerror=alert(1)>`, Graded: true, Score: 0.9},
		{ID: "p2", Address: "5.6.7.8:1080", Tier: "A", Graded: true, Score: 0.95},
		{ID: "p3", Address: "9.9.9.9:1080", Tier: "", Graded: false}, // ungraded, untouched
	}

	sanitizeProxyGrades(proxies)

	if proxies[0].Graded {
		t.Fatalf("crafted tier proxy still graded: %+v", proxies[0])
	}
	if proxies[0].Tier != "" || proxies[0].Score != 0 {
		t.Errorf("crafted tier not nulled: %+v", proxies[0])
	}
	if !proxies[1].Graded || proxies[1].Tier != "A" {
		t.Errorf("valid A grade must survive: %+v", proxies[1])
	}
	if proxies[2].Graded {
		t.Errorf("ungraded proxy must stay ungraded: %+v", proxies[2])
	}
}

func TestHandleReportSanitizesBeforeStore(t *testing.T) {
	s := newTestStore(t)

	body, _ := json.Marshal(nodeState{
		NodeID:    "n1",
		Timestamp: time.Now().UTC(),
		Proxies: []proxyReport{
			{ID: "p1", Address: "1.2.3.4:1080", Tier: `"><script>alert(1)</script>`, Graded: true, Score: 0.9},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/report", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleReport(s)(w, req)
	if w.Code != http.StatusNoContent && w.Code != http.StatusOK && w.Code != 204 {
		t.Fatalf("handleReport status = %d, want 204", w.Code)
	}

	// The drawer endpoint serves the in-memory n.Proxies — the crafted tier
	// must NOT be there.
	s.mu.RLock()
	node, ok := s.Nodes["n1"]
	s.mu.RUnlock()
	if !ok {
		t.Fatal("node not stored")
	}
	if len(node.Proxies) != 1 {
		t.Fatalf("proxies = %d, want 1", len(node.Proxies))
	}
	if node.Proxies[0].Graded {
		t.Fatalf("crafted-tier proxy survived into the in-memory store (drawer XSS path): %+v", node.Proxies[0])
	}
	if node.Proxies[0].Tier != "" {
		t.Errorf("tier not nulled: %q", node.Proxies[0].Tier)
	}
}
