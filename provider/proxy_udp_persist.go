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
	Key     string         `json:"key"`
	Status  proxyUDPStatus `json:"status"`
	At      time.Time      `json:"at"`
	TCPExit string         `json:"tcp_exit,omitempty"`
	UDPExit string         `json:"udp_exit,omitempty"`
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
	if json.Unmarshal(b, &state) != nil || state.Version != 1 || len(state.Entries) > udpStateMaxEntries {
		return
	}
	for _, entry := range state.Entries {
		key, err := hex.DecodeString(entry.Key)
		if err != nil || len(key) != sha256.Size || entry.Status < proxyUDPUnknown || entry.Status > proxyUDPExitMismatch || entry.At.IsZero() || entry.At.After(now) || now.Sub(entry.At) >= proxyUDPTTL(entry.Status) || (entry.TCPExit != "" && net.ParseIP(entry.TCPExit) == nil) || (entry.UDPExit != "" && net.ParseIP(entry.UDPExit) == nil) {
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
		entry := &proxyUDPCheckEntry{at: saved.At, tcpExit: saved.TCPExit, result: proxyUDPResult{Status: saved.Status, TCPExit: saved.TCPExit, UDPExit: saved.UDPExit}}
		self.entries[key] = entry
		return entry
	}
	return nil
}

// persistLocked serializes completed results, never raw identities or errors.
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
	self.persisted[hash] = udpPersistEntry{Key: hash, Status: entry.result.Status, At: entry.at, TCPExit: tcpExit, UDPExit: udpExit}
	list := make([]udpPersistEntry, 0, len(self.persisted))
	for hash, saved := range self.persisted {
		if saved.At.After(now) || now.Sub(saved.At) >= proxyUDPTTL(saved.Status) {
			delete(self.persisted, hash)
			continue
		}
		list = append(list, saved)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].At.Equal(list[j].At) {
			return list[i].Key < list[j].Key
		}
		return list[i].At.After(list[j].At)
	})
	if len(list) > udpStateMaxEntries {
		for _, dropped := range list[udpStateMaxEntries:] {
			delete(self.persisted, dropped.Key)
		}
		list = list[:udpStateMaxEntries]
	}
	b, err := json.Marshal(udpPersistFile{Version: 1, Entries: list})
	if err != nil {
		return err
	}
	for len(b) > udpStateMaxBytes && len(list) > 0 {
		delete(self.persisted, list[len(list)-1].Key)
		list = list[:len(list)-1]
		b, err = json.Marshal(udpPersistFile{Version: 1, Entries: list})
		if err != nil {
			return err
		}
	}
	if len(b) > udpStateMaxBytes {
		return io.ErrShortBuffer
	}
	if err := os.MkdirAll(filepath.Dir(self.persistPath), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(self.persistPath), ".udp-state-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), self.persistPath)
}
