package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

func proxyReloadPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy.reload"), nil
}

func proxyLockPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy.lock"), nil
}

// readReloadSeq reads the current sequence number from the trigger file.
// Returns 0 if the file does not exist.
func readReloadSeq(path string) (int, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		// Treat an unparseable trigger as seq 0 rather than failing the watcher.
		return 0, nil
	}
	return n, nil
}

// writeReloadTrigger increments the sequence number in the trigger file.
// Called by the proxy refresh subcommand after confirmation.
func writeReloadTrigger(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	seq, _ := readReloadSeq(path)
	seq++
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(seq)), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// acquireProxyLock creates the lock file at the default path. Returns an error
// if a reload is already in progress (lock already held).
func acquireProxyLock() (func(), error) {
	path, err := proxyLockPath()
	if err != nil {
		return nil, err
	}
	return acquireProxyLockAt(path)
}

// acquireProxyLockAt is the path-explicit form, for testing.
func acquireProxyLockAt(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("reload already in progress — try again in a moment")
		}
		return nil, err
	}
	f.Close()
	return func() { os.Remove(path) }, nil
}

// ProxyReloader manages hot-reload of proxy goroutines. It is driven by the
// reload watcher goroutine, which polls the trigger file for a changed sequence
// number and calls reload(). reload() is serialized by mu so two reloads never
// overlap.
type ProxyReloader struct {
	mu          sync.Mutex // serializes reloads
	cancelMap   map[string]context.CancelFunc
	cancelMapMu *sync.Mutex
	state       *ProxyState
	sourcePath  string // "" = internal config (~/.urnetwork/proxy); else external file
	parentCtx   context.Context
	wg          *sync.WaitGroup

	// spawnProxy starts a proxy goroutine's work (the provideWithProxy closure).
	spawnProxy func(proxyCtx context.Context, settings *connect.ProxySettings)
}

// StartWatcher launches the background goroutine that polls the reload trigger
// file every 2 seconds and triggers reload() when its sequence number changes.
func (r *ProxyReloader) StartWatcher(ctx context.Context) {
	reloadPath, err := proxyReloadPath()
	if err != nil {
		fmt.Printf("[proxy] warning: could not determine reload path: %v\n", err)
		return
	}

	lastSeq, _ := readReloadSeq(reloadPath)

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				seq, err := readReloadSeq(reloadPath)
				if err != nil || seq == lastSeq {
					continue
				}
				lastSeq = seq
				r.reload()
			}
		}
	}()
}

// reload diffs the proxy source against the currently running set and applies
// the difference: cancels goroutines for removed proxies, starts goroutines for
// added proxies (staggered), and rewrites proxy.state. Untouched proxies are
// never disturbed.
func (r *ProxyReloader) reload() {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Load desired set from the source. On a read error in Workflow A, SKIP the
	// reload entirely — proceeding would diff against zero proxies and cancel the
	// entire running fleet over a transient file error.
	var desired []*connect.ProxySettings
	if r.sourcePath != "" {
		settings, err := readProxySettingsFromFile(r.sourcePath)
		if err != nil {
			fmt.Printf("[proxy] reload skipped: could not read source: %v\n", err)
			return
		}
		desired = settings
	} else {
		desired = readProxySettings()
	}

	desiredSet := make(map[string]*connect.ProxySettings, len(desired))
	for _, s := range desired {
		desiredSet[s.Address] = s
	}

	// Snapshot the currently running set from the cancel map.
	r.cancelMapMu.Lock()
	running := make(map[string]bool, len(r.cancelMap))
	for addr := range r.cancelMap {
		running[addr] = true
	}
	r.cancelMapMu.Unlock()

	var added []*connect.ProxySettings
	for addr, s := range desiredSet {
		if !running[addr] {
			added = append(added, s)
		}
	}
	var removed []string
	for addr := range running {
		if _, ok := desiredSet[addr]; !ok {
			removed = append(removed, addr)
		}
	}

	// Cancel removed proxies and drop them from the cancel map and state.
	for _, addr := range removed {
		r.cancelMapMu.Lock()
		cancel, ok := r.cancelMap[addr]
		if ok {
			delete(r.cancelMap, addr)
		}
		r.cancelMapMu.Unlock()
		if ok {
			cancel()
		}
		delete(r.state.Proxies, addr)
	}

	// Start added proxies. Each goroutine staggers its own startup by 100ms *
	// position to avoid a burst of simultaneous connection attempts at the API.
	for i, settings := range added {
		stableID := resolveProxyID(r.state, settings.Address)
		settings.Index = stableID
		connect.RegisterProxy(stableID, settings.Address)

		proxyCtx, proxyCancel := context.WithCancel(r.parentCtx)
		r.cancelMapMu.Lock()
		r.cancelMap[settings.Address] = proxyCancel
		r.cancelMapMu.Unlock()

		staggerPos := i
		settingsCopy := settings
		r.wg.Add(1)
		go connect.HandleError(func() {
			defer r.wg.Done()
			defer connect.UnregisterProxy(stableID)
			defer proxyCancel()

			initialDelay := time.Duration(staggerPos) * 100 * time.Millisecond
			select {
			case <-proxyCtx.Done():
				return
			case <-time.After(initialDelay):
			}
			r.spawnProxy(proxyCtx, settingsCopy)
		})
	}

	// Persist the new state snapshot.
	r.state.NextID = currentProxyIDCounter()
	if err := writeProxyState(r.state); err != nil {
		fmt.Printf("[proxy] warning: could not write proxy.state after reload: %v\n", err)
	}

	fmt.Printf("[proxy] reloaded: +%d added, -%d removed\n", len(added), len(removed))
}
