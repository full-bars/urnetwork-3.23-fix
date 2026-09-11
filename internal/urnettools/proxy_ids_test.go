package urnettools

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/tabwriter"
	"time"
)

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s ago"},
		{5 * time.Minute, "5m ago"},
		{3 * time.Hour, "3h ago"},
		{2 * 24 * time.Hour, "2d ago"},
		{10 * 24 * time.Hour, "10d ago"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestRenderProxyIDs_Empty(t *testing.T) {
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 2, 8, 2, ' ', 0)
	entries := map[string]clientJWTEntryMinimal{}
	renderProxyIDs(w, entries)
	out := buf.String()
	if !strings.Contains(out, "(no entries)") {
		t.Errorf("expected '(no entries)' message, got:\n%s", out)
	}
}

func TestRenderProxyIDs_WithEntries(t *testing.T) {
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 2, 8, 2, ' ', 0)
	now := time.Now()
	entries := map[string]clientJWTEntryMinimal{
		"direct": {
			ClientID:  "01a0857c-8efd-e7cb-933c-54387553cef2",
			NetworkID: "0197d746-bd9d-43f3-d325-e609a2e70b2f",
			MintedAt:  now.Add(-5 * time.Minute),
		},
		"1.2.3.4:8080": {
			ClientID:  "aabbccdd-1111-2222-3333-444455556666",
			NetworkID: "11223344-5566-7788-99aa-bbccddeeff00",
			MintedAt:  now.Add(-2 * time.Hour),
		},
	}
	renderProxyIDs(w, entries)
	out := buf.String()

	// Should contain header
	if !strings.Contains(out, "PROXY") || !strings.Contains(out, "CLIENT_ID") {
		t.Errorf("missing table header in output:\n%s", out)
	}
	// "direct" should be rendered as "(direct)"
	if !strings.Contains(out, "(direct)") {
		t.Errorf("expected '(direct)' label, got:\n%s", out)
	}
	// Regular proxy address should appear as-is
	if !strings.Contains(out, "1.2.3.4:8080") {
		t.Errorf("expected '1.2.3.4:8080' in output, got:\n%s", out)
	}
}

func TestLoadClientJWTStore(t *testing.T) {
	dir := t.TempDir()
	content := `{
  "direct": {
    "client_id": "01a0857c-8efd-e7cb-933c-54387553cef2",
    "network_id": "0197d746-bd9d-43f3-d325-e609a2e70b2f",
    "minted_at": "2026-09-09T02:25:27.903056924-07:00",
    "by_client_jwt": "eyJhbG..."
  },
  "5.6.7.8:443": {
    "client_id": "aabbccdd-1111-2222-3333-444455556666",
    "network_id": "11223344-5566-7788-99aa-bbccddeeff00",
    "minted_at": "2026-09-10T10:00:00Z",
    "by_client_jwt": "eyJhbG..."
  }
}`
	path := filepath.Join(dir, ".client_jwts.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := loadClientJWTStore(dir)
	if err != nil {
		t.Fatalf("loadClientJWTStore: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	d, ok := entries["direct"]
	if !ok {
		t.Fatal("missing 'direct' entry")
	}
	if d.ClientID != "01a0857c-8efd-e7cb-933c-54387553cef2" {
		t.Errorf("direct ClientID = %q", d.ClientID)
	}
	if d.NetworkID != "0197d746-bd9d-43f3-d325-e609a2e70b2f" {
		t.Errorf("direct NetworkID = %q", d.NetworkID)
	}
	p, ok := entries["5.6.7.8:443"]
	if !ok {
		t.Fatal("missing '5.6.7.8:443' entry")
	}
	if p.ClientID != "aabbccdd-1111-2222-3333-444455556666" {
		t.Errorf("proxy ClientID = %q", p.ClientID)
	}
}

func TestLoadClientJWTStore_Missing(t *testing.T) {
	dir := t.TempDir()
	entries, err := loadClientJWTStore(dir)
	if err != nil {
		t.Fatalf("loadClientJWTStore on missing file: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty entries for missing file, got %d", len(entries))
	}
}

func TestLoadClientJWTStore_Corrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".client_jwts.json")
	if err := os.WriteFile(path, []byte("{bad json"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := loadClientJWTStore(dir)
	if err == nil {
		t.Fatal("expected error for corrupt JSON, got nil")
	}
}
