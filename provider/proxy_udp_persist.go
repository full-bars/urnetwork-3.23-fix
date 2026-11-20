package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const udpStateMaxBytes = 2 << 20
const udpStateMaxEntries = 10000

type udpPersistEntry struct {
	Key     string          `json:"key"`
	Status  proxyUDPStatus  `json:"status"`
	At      time.Time       `json:"at"`
	TCPExit string          `json:"tcp_exit,omitempty"`
	UDPExit string          `json:"udp_exit,omitempty"`
	IPv6    proxyIPv6Result `json:"ipv6,omitempty"`
}

type udpPersistFile struct {
	Version int               `json:"version"`
	Entries []udpPersistEntry `json:"entries"`
}

func udpPersistKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// enablePersistence loads a bounded snapshot before the store is used.
func (self *proxyUDPCheckStore) enablePersistence(path string, now time.Time) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.persistPath = path
	self.persisted = make(map[string]udpPersistEntry)
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, udpStateMaxBytes+1))
	if err != nil || len(b) > udpStateMaxBytes {
		return
	}
	var state udpPersistFile
	if json.Unmarshal(b, &state) != nil || (state.Version != 1 && state.Version != 2) || len(state.Entries) > udpStateMaxEntries {
		return
	}
	for _, entry := range state.Entries {
		if !entry.IPv6.valid(now) {
			entry.IPv6 = proxyIPv6Result{}
		}
		key, err := hex.DecodeString(entry.Key)
		if err != nil || len(key) != sha256.Size || entry.Status < proxyUDPUnknown || entry.Status > proxyUDPExitMismatch || entry.At.IsZero() || entry.At.After(now) || (now.Sub(entry.At) >= proxyUDPTTL(entry.Status) && !entry.IPv6.retain(now)) || (entry.TCPExit != "" && net.ParseIP(entry.TCPExit) == nil) || (entry.UDPExit != "" && net.ParseIP(entry.UDPExit) == nil) {
			continue
		}
		self.persisted[entry.Key] = entry
	}
}

// cachedEntryLocked restores a persisted identity on first use.
func (self *proxyUDPCheckStore) cachedEntryLocked(key string) *proxyUDPCheckEntry {
	if entry := self.entries[key]; entry != nil {
		return entry
	}
	if saved, ok := self.persisted[udpPersistKey(key)]; ok {
		entry := &proxyUDPCheckEntry{ipv6: saved.IPv6, at: saved.At, tcpExit: saved.TCPExit, result: proxyUDPResult{Status: saved.Status, TCPExit: saved.TCPExit, UDPExit: saved.UDPExit}}
		self.entries[key] = entry
		return entry
	}
	return nil
}

// udpPersistDelay is how long the background writer waits before it writes, so
// a burst of completed checks costs one file write instead of one each.
var udpPersistDelay = 2 * time.Second

// udpPersistWriteFile writes the state file. It is a variable so a test can
// observe or hold a write.
var udpPersistWriteFile = writeUDPPersistFile

// persistLocked records a completed result and schedules one background write.
// It only touches memory: the check path and the flow path hold self.mu, so they
// must never wait for a disk. The file holds results, never raw identities or
// errors.
func (self *proxyUDPCheckStore) persistLocked(key string, entry *proxyUDPCheckEntry, now time.Time) error {
	if self.persistPath == "" {
		return nil
	}
	if self.persisted == nil {
		self.persisted = make(map[string]udpPersistEntry)
	}
	tcpExit, udpExit := entry.tcpExit, entry.result.UDPExit
	if net.ParseIP(tcpExit) == nil {
		tcpExit = ""
	}
	if net.ParseIP(udpExit) == nil {
		udpExit = ""
	}
	hash := udpPersistKey(key)
	self.persisted[hash] = udpPersistEntry{Key: hash, Status: entry.result.Status, At: entry.at, TCPExit: tcpExit, UDPExit: udpExit, IPv6: entry.ipv6}
	self.persistDirty = true
	// Checks for different identities finish in any order. The writer drops
	// entries dated after persistNow, so it must never move backwards.
	if now.After(self.persistNow) {
		self.persistNow = now
	}
	if self.persistKick == nil {
		self.persistKick = make(chan struct{}, 1)
	}
	if !self.persistWriting {
		self.persistWriting = true
		self.persistDone = make(chan struct{})
		go self.persistWriter()
	}
	return nil
}

// persistWriter writes the state file until nothing is left to write, then
// exits. At most one runs per store.
func (self *proxyUDPCheckStore) persistWriter() {
	for {
		timer := time.NewTimer(udpPersistDelay)
		select {
		case <-timer.C:
		case <-self.persistKick:
			timer.Stop()
		}
		self.mu.Lock()
		self.persistDirty = false
		now, path := self.persistNow, self.persistPath
		list := make([]udpPersistEntry, 0, len(self.persisted))
		snapshot := make(map[string]udpPersistEntry, len(self.persisted))
		for hash, saved := range self.persisted {
			if saved.At.After(now) || (now.Sub(saved.At) >= proxyUDPTTL(saved.Status) && !saved.IPv6.retain(now)) {
				delete(self.persisted, hash)
				continue
			}
			list = append(list, saved)
			snapshot[hash] = saved
		}
		self.mu.Unlock()

		dropped, err := udpPersistWriteFile(path, list)
		self.mu.Lock()
		// Entries beyond the file limits are forgotten after every attempt, also a
		// failed one, so a write that keeps failing cannot hold them in memory. A
		// newer result for the same key stays.
		for _, hash := range dropped {
			if saved, ok := snapshot[hash]; ok && self.persisted[hash] == saved {
				delete(self.persisted, hash)
			}
		}
		if err != nil {
			self.persistDirty = true
			if !self.persistFailed {
				tlog("[proxy][udp] could not persist probe state\n")
			}
			self.persistFailed = true
		} else {
			self.persistFailed = false
		}
		if !self.persistDirty {
			self.persistWriting = false
			close(self.persistDone)
			self.persistDone = nil
			self.mu.Unlock()
			return
		}
		self.mu.Unlock()
	}
}

// flushPersist writes any pending state now and waits for the writer to finish.
func (self *proxyUDPCheckStore) flushPersist() {
	self.mu.Lock()
	done := self.persistDone
	if done != nil {
		select {
		case self.persistKick <- struct{}{}:
		default:
		}
	}
	self.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (self *proxyUDPCheckStore) flushPersistWithin(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		self.flushPersist()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// writeUDPPersistFile sorts the entries newest first, keeps the newest ones
// within the entry and byte limits, and replaces the file atomically. It
// returns the keys it dropped so the caller can forget them too.
func writeUDPPersistFile(path string, list []udpPersistEntry) (dropped []string, err error) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].At.Equal(list[j].At) {
			return list[i].Key < list[j].Key
		}
		return list[i].At.After(list[j].At)
	})
	if len(list) > udpStateMaxEntries {
		for _, d := range list[udpStateMaxEntries:] {
			dropped = append(dropped, d.Key)
		}
		list = list[:udpStateMaxEntries]
	}
	// The encoded size of the file is the size of the empty file plus each
	// entry and one comma between entries. Sum it once and cut the list at the
	// last entry that fits, instead of encoding the whole list again per entry.
	base, err := json.Marshal(udpPersistFile{Version: 2, Entries: []udpPersistEntry{}})
	if err != nil {
		return dropped, err
	}
	size := len(base)
	fit := 0
	for i := range list {
		e, err := json.Marshal(list[i])
		if err != nil {
			return dropped, err
		}
		next := size + len(e)
		if i > 0 {
			next++
		}
		if next > udpStateMaxBytes {
			break
		}
		size = next
		fit = i + 1
	}
	for _, d := range list[fit:] {
		dropped = append(dropped, d.Key)
	}
	list = list[:fit]
	b, err := json.Marshal(udpPersistFile{Version: 2, Entries: list})
	if err != nil {
		return dropped, err
	}
	if len(b) > udpStateMaxBytes {
		return dropped, io.ErrShortBuffer
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return dropped, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".udp-state-*.tmp")
	if err != nil {
		return dropped, err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return dropped, err
	}
	if err := f.Close(); err != nil {
		return dropped, err
	}
	return dropped, os.Rename(f.Name(), path)
}
