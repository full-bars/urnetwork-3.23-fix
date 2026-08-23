package connect

import (
	"context"
	"testing"
)

// TestManagerWiresPQETracker guards the CRITICAL dead-feature regression: the
// EncryptionSessionManager must initialize its pqeTracker so NoteOpen/Close and
// PQECounts take effect. It was declared but never assigned, which left the
// [pqe] log permanently zero. newPQETracker is called in the constructor, so a
// freshly built manager must not return a nil tracker through PQECounts.
func TestManagerWiresPQETracker(t *testing.T) {
	// EncryptionModeOff settings avoids TLS config setup; the constructor only
	// needs the handle to exist to call newPQETracker() (it does not read the
	// client/key manager for that path).
	settings := DefaultEncryptionSettings()
	settings.Mode = EncryptionModeOff
	m := NewEncryptionSessionManager(context.Background(), &Client{}, nil, settings)
	if m == nil {
		t.Fatal("nil manager")
	}
	if m.pqeTracker == nil {
		t.Fatal("pqeTracker is nil - PQE visibility is a dead feature")
	}
	// NoteOpen on a nil-settings manager is safe; confirm counts update.
	m.pqeTracker.NoteOpen(true)
	c := m.PQECounts()
	if c.ActivePQE != 1 || c.PQEHour < 1 || c.PQELifetime < 1 {
		t.Fatalf("PQECounts after one open: active=%d hour=%d life=%d, want 1/>=1/>=1", c.ActivePQE, c.PQEHour, c.PQELifetime)
	}
}
