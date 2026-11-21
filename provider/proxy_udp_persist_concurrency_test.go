package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUDPPersistKeepsNewerDroppedEntry(t *testing.T) {
	oldWrite, oldDelay := udpPersistWriteFile, udpPersistDelay
	t.Cleanup(func() { udpPersistWriteFile, udpPersistDelay = oldWrite, oldDelay })
	udpPersistDelay = 0
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	udpPersistWriteFile = func(_ string, list []udpPersistEntry) ([]string, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return []string{list[0].Key}, nil
		}
		return nil, nil
	}
	now := time.Now()
	store := newProxyUDPCheckStore(nil)
	store.enablePersistence(filepath.Join(t.TempDir(), "state"), now)
	store.mu.Lock()
	store.persistLocked("key", &proxyUDPCheckEntry{at: now, result: proxyUDPResult{Status: proxyUDPOK}}, now)
	store.mu.Unlock()
	waitSignal(t, entered, "write")
	newer := now.Add(time.Second)
	store.mu.Lock()
	store.persistLocked("key", &proxyUDPCheckEntry{at: newer, result: proxyUDPResult{Status: proxyUDPNoRelay}}, newer)
	store.mu.Unlock()
	close(release)
	store.flushPersist()
	store.mu.Lock()
	saved, ok := store.persisted[udpPersistKey("key")]
	store.mu.Unlock()
	if !ok || !saved.At.Equal(newer) || saved.Status != proxyUDPNoRelay {
		t.Fatal("newer entry lost")
	}
}

func TestUDPPersistRetriesFailure(t *testing.T) {
	oldWrite, oldDelay := udpPersistWriteFile, udpPersistDelay
	t.Cleanup(func() { udpPersistWriteFile, udpPersistDelay = oldWrite, oldDelay })
	udpPersistDelay = time.Millisecond
	var calls atomic.Int32
	udpPersistWriteFile = func(_ string, _ []udpPersistEntry) ([]string, error) {
		if calls.Add(1) < 3 {
			return nil, errors.New("write failed")
		}
		return nil, nil
	}
	now := time.Now()
	store := newProxyUDPCheckStore(nil)
	store.enablePersistence(filepath.Join(t.TempDir(), "state"), now)
	store.mu.Lock()
	store.persistLocked("key", &proxyUDPCheckEntry{at: now, result: proxyUDPResult{Status: proxyUDPOK}}, now)
	store.mu.Unlock()
	store.flushPersist()
	if calls.Load() != 3 {
		t.Fatalf("writes: %d", calls.Load())
	}
	store.mu.Lock()
	failed, dirty := store.persistFailed, store.persistDirty
	store.mu.Unlock()
	if failed || dirty {
		t.Fatal("writer did not recover")
	}
}

func TestUDPPersistFlushBounded(t *testing.T) {
	oldWrite, oldDelay := udpPersistWriteFile, udpPersistDelay
	t.Cleanup(func() { udpPersistWriteFile, udpPersistDelay = oldWrite, oldDelay })
	udpPersistDelay = 0
	entered, release := make(chan struct{}), make(chan struct{})
	// the writer runs in a goroutine that can outlive one test, so the signal
	// must be idempotent: a second write used to panic on the closed channel
	var enteredOnce sync.Once
	udpPersistWriteFile = func(_ string, _ []udpPersistEntry) ([]string, error) {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return nil, nil
	}
	now := time.Now()
	store := newProxyUDPCheckStore(nil)
	store.enablePersistence(filepath.Join(t.TempDir(), "state"), now)
	store.mu.Lock()
	store.persistLocked("key", &proxyUDPCheckEntry{at: now, result: proxyUDPResult{Status: proxyUDPOK}}, now)
	store.mu.Unlock()
	waitSignal(t, entered, "write")
	if store.flushPersistWithin(time.Millisecond) {
		t.Fatal("blocked write returned")
	}
	close(release)
	store.flushPersist()
}

func TestUDPPersistConcurrentFlush(t *testing.T) {
	oldWrite, oldDelay := udpPersistWriteFile, udpPersistDelay
	t.Cleanup(func() { udpPersistWriteFile, udpPersistDelay = oldWrite, oldDelay })
	udpPersistDelay = 0
	udpPersistWriteFile = func(_ string, _ []udpPersistEntry) ([]string, error) { return nil, nil }
	now := time.Now()
	store := newProxyUDPCheckStore(nil)
	store.enablePersistence(filepath.Join(t.TempDir(), "state"), now)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				store.mu.Lock()
				store.persistLocked(fmt.Sprintf("%d-%d", g, i), &proxyUDPCheckEntry{at: now, result: proxyUDPResult{Status: proxyUDPOK}}, now)
				store.mu.Unlock()
				store.flushPersist()
			}
		}(g)
	}
	wg.Wait()
	store.flushPersist()
}

// A write that fails still reports which entries are beyond the file limits.
// They must leave memory after that attempt, so a failure that keeps repeating
// cannot hold them until their TTL expires.
func TestUDPPersistDropsOverLimitEntriesAfterAFailedWrite(t *testing.T) {
	oldWrite, oldDelay := udpPersistWriteFile, udpPersistDelay
	t.Cleanup(func() { udpPersistWriteFile, udpPersistDelay = oldWrite, oldDelay })
	udpPersistDelay = 0
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var dropped []string
	udpPersistWriteFile = func(_ string, list []udpPersistEntry) ([]string, error) {
		if calls.Add(1) == 1 {
			dropped = []string{list[0].Key}
			return dropped, errors.New("disk full")
		}
		close(entered)
		<-release
		return nil, nil
	}
	now := time.Now()
	store := newProxyUDPCheckStore(nil)
	store.enablePersistence(filepath.Join(t.TempDir(), "state"), now)
	store.mu.Lock()
	for i := 0; i < 3; i++ {
		store.persistLocked(fmt.Sprintf("key-%d", i), &proxyUDPCheckEntry{at: now, result: proxyUDPResult{Status: proxyUDPOK}}, now)
	}
	store.mu.Unlock()
	waitSignal(t, entered, "retry write")
	store.mu.Lock()
	_, stillThere := store.persisted[dropped[0]]
	size := len(store.persisted)
	store.mu.Unlock()
	close(release)
	store.flushPersist()
	if stillThere || size != 2 {
		t.Fatalf("the dropped entry stayed after a failed write: present=%v size=%d", stillThere, size)
	}
}
