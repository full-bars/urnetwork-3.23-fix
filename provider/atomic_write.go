package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// atomicWriteJSON writes data as a JSON file atomically: temp + fsync + rename,
// with a checksum envelope and .bak backup for crash recovery.
//
// File format:
//
//	{
//	  "data": <actual payload>,
//	  "checksum": "sha256:<hex>",
//	  "saved_at": "2006-01-02T15:04:05Z07:00"
//	}
//
// Recovery order: path → path.bak → quarantine corrupt → defaults.
func atomicWriteJSON(path string, data interface{}) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("atomicWriteJSON mkdir: %w", err)
	}

	// Compute checksum from compact JSON — this is what verification uses.
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("atomicWriteJSON marshal: %w", err)
	}
	checksum := fmt.Sprintf("sha256:%x", sha256.Sum256(raw))

	// Store raw bytes in envelope so tryLoadJSON can verify checksum
	// against the exact same bytes (indentation-independent).
	envelope := map[string]interface{}{
		"data":     json.RawMessage(raw),
		"checksum": checksum,
		"saved_at": time.Now().UTC().Format(time.RFC3339),
	}

	blob, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return fmt.Errorf("atomicWriteJSON envelope: %w", err)
	}
	blob = append(blob, '\n')

	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("atomicWriteJSON create tmp: %w", err)
	}

	if _, err := f.Write(blob); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("atomicWriteJSON write: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("atomicWriteJSON sync: %w", err)
	}
	f.Close()

	// Backup last known good (best-effort)
	os.Remove(path + ".bak")
	os.Rename(path, path+".bak")

	// Atomic rename
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("atomicWriteJSON rename: %w", err)
	}

	// Fsync parent directory for durability
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		dir.Sync()
		dir.Close()
	}

	return nil
}

// loadJSONWithRecovery attempts to load a JSON file with checksum verification,
// falling back to .bak, then quarantining any corrupt file.
// Returns (data, true) on success, (nil, false) if no valid file found.
func loadJSONWithRecovery(path string, target interface{}) (bool, error) {
	// Try primary file
	if ok, err := tryLoadJSON(path, target); ok || err != nil {
		return ok, err
	}

	// Try backup
	bak := path + ".bak"
	if ok, err := tryLoadJSON(bak, target); ok || err != nil {
		if ok {
			// Restore backup as primary
			os.Rename(bak, path)
		}
		return ok, err
	}

	// Quarantine corrupt primary if it exists
	if _, err := os.Stat(path); err == nil {
		quarantine := fmt.Sprintf("%s.corrupt.%d", path, time.Now().Unix())
		os.Rename(path, quarantine)
		tlog("⚠️ [persist] quarantined corrupt %s → %s\n", path, quarantine)
	}

	return false, nil
}

// tryLoadJSON reads a JSON file and verifies its checksum envelope.
func tryLoadJSON(path string, target interface{}) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	var envelope struct {
		Data     json.RawMessage `json:"data"`
		Checksum string          `json:"checksum"`
		SavedAt  string          `json:"saved_at"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return false, nil // corrupt, not an error we propagate
	}

	// Re-compact the data to match the checksum (MarshalIndent changes
	// whitespace of embedded RawMessage, so we re-marshal to compact).
	compact, err := json.Marshal(envelope.Data)
	if err != nil {
		return false, nil
	}
	expected := fmt.Sprintf("sha256:%x", sha256.Sum256(compact))
	if envelope.Checksum != expected {
		return false, nil // checksum mismatch
	}

	if err := json.Unmarshal(envelope.Data, target); err != nil {
		return false, nil
	}

	return true, nil
}
