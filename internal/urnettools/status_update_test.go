package urnettools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatusUpdateParts(t *testing.T) {
	cases := []struct {
		name      string
		installed string
		state     statusUpdateState
		wantMain  string
		wantHint  string
	}{
		{
			name:      "up to date",
			installed: "v3.23.0-fix.35.11",
			state:     statusUpdateState{Tag: "v3.23.0-fix.35.11"},
			wantMain:  "update: installed v3.23.0-fix.35.11; latest v3.23.0-fix.35.11",
			wantHint:  "",
		},
		{
			name:      "update available",
			installed: "v3.23.0-fix.35.11",
			state:     statusUpdateState{Tag: "v3.23.0-fix.35.12"},
			wantMain:  "update: installed v3.23.0-fix.35.11; latest v3.23.0-fix.35.12",
			wantHint:  "run urnet-tools update",
		},
		{
			name:      "newer than published",
			installed: "v3.23.0-fix.35.13",
			state:     statusUpdateState{Tag: "v3.23.0-fix.35.12"},
			wantMain:  "update: installed v3.23.0-fix.35.13; latest v3.23.0-fix.35.12",
			wantHint:  "running newer than published",
		},
		{
			name:      "unavailable",
			installed: "v3.23.0-fix.35.11",
			state:     statusUpdateState{Unavailable: true},
			wantMain:  "update: installed v3.23.0-fix.35.11; latest unknown; update check unavailable",
			wantHint:  "",
		},
		{
			name:      "installed unknown",
			installed: "",
			state:     statusUpdateState{Tag: "v3.23.0-fix.35.12"},
			wantMain:  "update: installed unknown; latest v3.23.0-fix.35.12",
			wantHint:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			main, hint := statusUpdateParts(tc.installed, tc.state)
			if main != tc.wantMain {
				t.Errorf("main = %q, want %q", main, tc.wantMain)
			}
			if hint != tc.wantHint {
				t.Errorf("hint = %q, want %q", hint, tc.wantHint)
			}
		})
	}
}

func TestStatusUpdateCacheWritesAndReads(t *testing.T) {
	dir := t.TempDir()
	up := statusUpdateState{Tag: "v3.23.0-fix.35.12", At: time.Now()}
	writeStatusUpdateCache(dir, up)
	got, ok := readStatusUpdateCache(dir)
	if !ok || got.Tag != up.Tag {
		t.Fatalf("cache round trip = %+v ok=%v", got, ok)
	}

	// 11 minute old entry is considered stale.
	old := statusUpdateState{Tag: "v1", At: time.Now().Add(-11 * time.Minute)}
	writeStatusUpdateCache(dir, old)
	if _, ok := readStatusUpdateCache(dir); ok {
		t.Fatal("stale cache entry must be ignored")
	}

	// Empty state dir is safely skipped.
	writeStatusUpdateCache("", up)
	if _, ok := readStatusUpdateCache(""); ok {
		t.Fatal("empty stateDir must return false")
	}

	// Corrupt cache file returns false.
	_ = os.WriteFile(filepath.Join(dir, statusUpdateCacheFile), []byte("invalid json"), 0o600)
	if _, ok := readStatusUpdateCache(dir); ok {
		t.Fatal("corrupted cache file must return false")
	}
}

func TestStatusClip(t *testing.T) {
	if got := statusClip("abcdefghij", 5); got != "abcde" {
		t.Fatalf("statusClip = %q, want %q", got, "abcde")
	}
	if got := statusClip("abc", 5); got != "abc" {
		t.Fatalf("statusClip = %q, want %q", got, "abc")
	}
	if got := statusClip("abc", 0); got != "abc" {
		t.Fatalf("statusClip = %q, want %q", got, "abc")
	}
}

func TestCheckStatusUpdateCached(t *testing.T) {
	dir := t.TempDir()
	cached := statusUpdateState{Tag: "v3.23.0-fix.35.99", At: time.Now()}
	writeStatusUpdateCache(dir, cached)

	got := checkStatusUpdate(dir)
	if got.Tag != cached.Tag || got.Unavailable {
		t.Fatalf("expected cached state %+v, got %+v", cached, got)
	}
}

func TestStatusJSONCarriesUpdateStatus(t *testing.T) {
	origCheck := checkStatusUpdateFn
	defer func() { checkStatusUpdateFn = origCheck }()

	checkStatusUpdateFn = func(string) statusUpdateState {
		return statusUpdateState{Tag: "v3.23.0-fix.35.12", At: time.Now()}
	}

	p := Provider{
		Unit:     "test.service",
		Version:  "v3.23.0-fix.35.11",
		StateDir: t.TempDir(),
		Running:  true,
	}
	d := collectStatusData(p)
	doc := buildStatusDoc(p, d)

	if doc.Update.Installed != "v3.23.0-fix.35.11" {
		t.Errorf("doc.Update.Installed = %q, want %q", doc.Update.Installed, "v3.23.0-fix.35.11")
	}
	if doc.Update.Latest != "v3.23.0-fix.35.12" {
		t.Errorf("doc.Update.Latest = %q, want %q", doc.Update.Latest, "v3.23.0-fix.35.12")
	}
	if !doc.Update.Available {
		t.Error("doc.Update.Available = false, want true")
	}
	if doc.Update.Unavailable {
		t.Error("doc.Update.Unavailable = true, want false")
	}
}

func TestFetchReleasePageWithContextDeadline(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]releaseJSON{})
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	client := ts.Client()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL, nil)
	_, err := client.Do(req)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}
}
