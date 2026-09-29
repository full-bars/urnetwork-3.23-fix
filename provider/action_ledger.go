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
// rotation does not happen on every append). The whole append+rotate runs under
// an inter-process lock (path+".lock"): during a HotSwap the parent and the
// candidate both record decisions, and an unlocked rotation could clobber the
// shared temp file or drop an entry appended after its snapshot.
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
	// Blocking flock, released when the holder dies, so there is no stale-lock
	// case to handle and a contended append waits instead of being dropped.
	release, err := acquireJWTStoreLock(path)
	if err != nil {
		return err
	}
	defer release()
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
			// The newest line alone is bigger than what we are allowed to keep.
			// Without this guard start stays len(b) and we would write an EMPTY
			// ledger, silently destroying the whole audit trail of a
			// self-managing agent (and ledgerRecord discards this error, so
			// nothing would ever be logged). Keep the oversized newest line
			// instead: over the bound for one entry beats losing all of them.
			//
			// j is the index of the newline BEFORE this line, so j+1 is where
			// this line starts. Using 0 here would keep the whole file and
			// never trim anything.
			if size == 0 {
				start = j + 1
			}
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

// ledgerRecordHook, when set by a test, runs at the start of ledgerRecord, so a
// test can see the lock state at the moment the ledger write happens instead of
// guessing it. The events.log writes moved out from under the reloader lock for
// exactly this reason (a blocking flock plus a 256 KiB rewrite is not something
// to hold a pool-wide mutex for), and the ledger writes are on the same paths.
var ledgerRecordHook func()

// ledgerRecord appends to the default ledger, ignoring errors: auditing must
// never block or fail the action it records.
func ledgerRecord(e ledgerEntry) {
	if ledgerRecordHook != nil {
		ledgerRecordHook()
	}
	if dir, err := oomCapDir(); err == nil {
		_ = ledgerAppend(filepath.Join(dir, ledgerFileName), e, time.Now(), ledgerMaxBytes)
	}
}
