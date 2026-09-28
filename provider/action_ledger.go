package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// action_ledger.go is an append-only, size-bounded JSONL record of every
// AUTOMATIC or operator-issued capacity decision (the OOM-aware cap, trim
// results), in one timeline at ~/.urnetwork/autopilot.jsonl. The RAM log
// rotates and is noisy; an autonomous system is only trustworthy if each thing
// it did can be audited later.

const (
	ledgerFileName = "autopilot.jsonl"
	ledgerMaxBytes = 256 << 10
)

type ledgerEntry struct {
	Time   string `json:"t"`
	Actor  string `json:"actor"`  // oomcap | trim
	Action string `json:"action"` // reduce | relax | clear | frozen | applied | cleared
	From   int    `json:"from"`
	To     int    `json:"to"`
	Mode   string `json:"mode,omitempty"` // shadow | on | operator
	Reason string `json:"reason,omitempty"`
}

// ledgerAppend adds one entry and, when the file would exceed maxBytes, rewrites
// it keeping only the newest whole lines that fit in half the bound (so
// rotation does not happen on every append).
func ledgerAppend(path string, e ledgerEntry, now time.Time, maxBytes int64) error {
	e.Time = now.UTC().Format(time.RFC3339)
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(line)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	if cerr != nil {
		return cerr
	}
	if st, err := os.Stat(path); err == nil && st.Size() > maxBytes {
		return ledgerTrim(path, maxBytes/2)
	}
	return nil
}

// ledgerTrim keeps the newest whole lines totalling at most keepBytes.
func ledgerTrim(path string, keepBytes int64) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	start := len(b)
	var size int64
	for i := len(b) - 1; i >= 0; {
		// find the start of the line ending at i
		j := i - 1
		for j >= 0 && b[j] != '\n' {
			j--
		}
		lineLen := int64(i - j)
		if size+lineLen > keepBytes {
			break
		}
		size += lineLen
		start = j + 1
		i = j
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b[start:], 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ledgerTail returns the newest n entries in chronological order. A missing file
// is empty; a line that does not parse is skipped.
func ledgerTail(path string, n int) ([]ledgerEntry, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var all []ledgerEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e ledgerEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Actor != "" {
			all = append(all, e)
		}
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, sc.Err()
}

// ledgerRecord appends to the default ledger, ignoring errors: auditing must
// never block or fail the action it records.
func ledgerRecord(e ledgerEntry) {
	if dir, err := oomCapDir(); err == nil {
		_ = ledgerAppend(filepath.Join(dir, ledgerFileName), e, time.Now(), ledgerMaxBytes)
	}
}
