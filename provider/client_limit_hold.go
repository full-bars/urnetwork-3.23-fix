package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

const clientLimitHoldFileName = "client_limit_hold.json"

// clientLimitHoldFile maps client id string to retry unix timestamp.
type clientLimitHoldFile map[string]int64

// clientLimitHoldEntry is one client's hold, shared by every user of that
// client id. The persist watcher lives only while refs > 0, so a proxy that is
// removed or reloaded does not leave a goroutine and a map entry behind.
type clientLimitHoldEntry struct {
	b    *connect.ClientLimitBackoff
	refs int
	stop chan struct{}
}

// clientLimitHoldWatchers counts live persist watchers, so a reset can wait
// until none can write any more.
var clientLimitHoldWatchers sync.WaitGroup

var clientLimitHolds = struct {
	sync.Mutex
	m map[string]*clientLimitHoldEntry
}{
	m: make(map[string]*clientLimitHoldEntry),
}

// clientLimitHoldPath returns the path to client_limit_hold.json in oomCapDir().
func clientLimitHoldPath() (string, error) {
	dir, err := oomCapDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, clientLimitHoldFileName), nil
}

// clientLimitHoldFor returns the shared ClientLimitBackoff for clientId with no
// lifetime: the caller never releases it. Production callers use
// clientLimitHoldWithContext.
func clientLimitHoldFor(clientId connect.Id, now time.Time) *connect.ClientLimitBackoff {
	return clientLimitHoldWithContext(context.Background(), clientId, now)
}

// clientLimitHoldWithContext returns the shared ClientLimitBackoff for
// clientId, restoring a standing hold from disk on first creation and starting
// its persist watcher. The caller holds one reference until ctx ends; when the
// last reference goes the watcher stops, the standing hold is flushed to disk
// and the entry is removed, so a replacement restores it.
func clientLimitHoldWithContext(ctx context.Context, clientId connect.Id, now time.Time) *connect.ClientLimitBackoff {
	key := clientId.String()

	// The hold file is read before the registry lock is taken, so 500 proxies
	// starting together do not serialize their disk reads behind one mutex. A
	// reference to an entry that already exists skips the read entirely.
	clientLimitHolds.Lock()
	entry, ok := clientLimitHolds.m[key]
	if ok {
		entry.refs += 1
		clientLimitHolds.Unlock()
	} else {
		clientLimitHolds.Unlock()

		var restoreUnix int64
		if path, err := clientLimitHoldPath(); err == nil {
			file := make(clientLimitHoldFile)
			if oomReadJSON(path, &file) {
				if retryUnix, ok := file[key]; ok && retryUnix > now.Unix() {
					restoreUnix = retryUnix
				}
			}
		}

		clientLimitHolds.Lock()
		if clientLimitHolds.m == nil {
			clientLimitHolds.m = make(map[string]*clientLimitHoldEntry)
		}
		if entry, ok = clientLimitHolds.m[key]; ok {
			// another caller created it while the file was being read
			entry.refs += 1
		} else {
			entry = &clientLimitHoldEntry{
				b:    connect.NewClientLimitBackoff(),
				refs: 1,
				stop: make(chan struct{}),
			}
			if restoreUnix != 0 {
				entry.b.Restore(time.Unix(restoreUnix, 0))
			}
			clientLimitHolds.m[key] = entry
			clientLimitHoldWatchers.Add(1)
			go runClientLimitHoldPersistWatcher(key, entry.b, entry.stop)
		}
		clientLimitHolds.Unlock()
	}

	if done := ctx.Done(); done != nil {
		go func() {
			<-done
			releaseClientLimitHold(key, entry)
		}()
	}

	return entry.b
}

// releaseClientLimitHold drops one reference. The last one removes the entry
// and stops its watcher, which writes the standing hold once more before it
// exits so a replacement restores it. No I/O happens under the registry lock.
func releaseClientLimitHold(key string, entry *clientLimitHoldEntry) {
	clientLimitHolds.Lock()
	entry.refs -= 1
	last := entry.refs <= 0 && clientLimitHolds.m[key] == entry
	if last {
		delete(clientLimitHolds.m, key)
	}
	clientLimitHolds.Unlock()

	if last {
		close(entry.stop)
	}
}

// runClientLimitHoldPersistWatcher writes the hold to disk when it starts,
// changes or ends. It writes nothing while the hold has never been in force, so
// a fleet of idle clients does not rewrite the shared file at startup. When
// stop closes it writes a standing hold once more and returns.
func runClientLimitHoldPersistWatcher(clientId string, b *connect.ClientLimitBackoff, stop <-chan struct{}) {
	defer clientLimitHoldWatchers.Done()
	persisted := false
	for {
		st, ch := b.Get()
		if st.Exceeded || persisted {
			_ = persistClientLimitHold(clientId, st, time.Now())
			persisted = st.Exceeded
		}
		select {
		case <-stop:
			// the hold may have changed after the read above
			if st := b.Status(); st.Exceeded || persisted {
				_ = persistClientLimitHold(clientId, st, time.Now())
			}
			return
		case <-ch:
		}
	}
}

// persistClientLimitHold persists the hold status of clientId to client_limit_hold.json.
// Under JWT store lock: prunes expired entries, updates clientId (keeping max retry time
// if already exceeded, or deleting if not exceeded), and writes atomically.
func persistClientLimitHold(clientId string, st connect.ClientLimitStatus, now time.Time) error {
	path, err := clientLimitHoldPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	release, err := acquireJWTStoreLock(path)
	if err != nil {
		return err
	}
	defer release()

	file := make(clientLimitHoldFile)
	_ = oomReadJSON(path, &file)
	if file == nil {
		file = make(clientLimitHoldFile)
	}

	// prune expired entries
	for k, retryUnix := range file {
		if retryUnix <= now.Unix() {
			delete(file, k)
		}
	}

	if st.Exceeded {
		retryUnix := st.RetryTime.Unix()
		if existing, ok := file[clientId]; ok && existing > retryUnix {
			retryUnix = existing
		}
		file[clientId] = retryUnix
	} else {
		delete(file, clientId)
	}

	return oomWriteJSONLocked(path, file)
}

// reloadClientLimitHolds restores every registered backoff from file.
func reloadClientLimitHolds(now time.Time) {
	path, err := clientLimitHoldPath()
	if err != nil {
		return
	}

	file := make(clientLimitHoldFile)
	if !oomReadJSON(path, &file) || file == nil {
		return
	}

	clientLimitHolds.Lock()
	defer clientLimitHolds.Unlock()

	for clientId, entry := range clientLimitHolds.m {
		if retryUnix, ok := file[clientId]; ok && retryUnix > now.Unix() {
			entry.b.Restore(time.Unix(retryUnix, 0))
		}
	}
}

// flushClientLimitHolds writes standing holds to disk synchronously before exit.
func flushClientLimitHolds() {
	clientLimitHolds.Lock()
	type snapshot struct {
		id string
		st connect.ClientLimitStatus
	}
	var snaps []snapshot
	for id, entry := range clientLimitHolds.m {
		snaps = append(snaps, snapshot{id: id, st: entry.b.Status()})
	}
	clientLimitHolds.Unlock()

	now := time.Now()
	for _, snap := range snaps {
		_ = persistClientLimitHold(snap.id, snap.st, now)
	}
}

func resetClientLimitHoldsForTest() {
	clientLimitHolds.Lock()
	for _, entry := range clientLimitHolds.m {
		close(entry.stop)
	}
	clientLimitHolds.m = make(map[string]*clientLimitHoldEntry)
	clientLimitHolds.Unlock()
	clientLimitHoldWatchers.Wait()
}

// provideIntentEnabled reports whether provider intent declaration is enabled.
// Default on; URNETWORK_PROVIDE_INTENT=0 disables.
func provideIntentEnabled() bool {
	if os.Getenv("URNETWORK_PROVIDE_INTENT") == "0" {
		return false
	}
	return true
}

// newProviderClientAuth builds a ClientAuth declaring provide intent according
// to provideIntentEnabled().
func newProviderClientAuth(byClientJwt string, instanceId connect.Id) *connect.ClientAuth {
	return &connect.ClientAuth{
		ByJwt:         byClientJwt,
		InstanceId:    instanceId,
		AppVersion:    RequireVersion(),
		ProvideIntent: provideIntentEnabled(),
	}
}
