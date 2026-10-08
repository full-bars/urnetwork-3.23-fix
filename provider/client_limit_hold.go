package main

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

const clientLimitHoldFileName = "client_limit_hold.json"

// clientLimitHoldFile maps client id string to retry unix timestamp.
type clientLimitHoldFile map[string]int64

var clientLimitHolds = struct {
	sync.Mutex
	m map[string]*connect.ClientLimitBackoff
}{
	m: make(map[string]*connect.ClientLimitBackoff),
}

// clientLimitHoldPath returns the path to client_limit_hold.json in oomCapDir().
func clientLimitHoldPath() (string, error) {
	dir, err := oomCapDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, clientLimitHoldFileName), nil
}

// clientLimitHoldFor returns the registered ClientLimitBackoff for clientId,
// restoring from disk on first creation and starting a persist watcher.
func clientLimitHoldFor(clientId connect.Id, now time.Time) *connect.ClientLimitBackoff {
	key := clientId.String()

	clientLimitHolds.Lock()
	defer clientLimitHolds.Unlock()

	if clientLimitHolds.m == nil {
		clientLimitHolds.m = make(map[string]*connect.ClientLimitBackoff)
	}

	if b, ok := clientLimitHolds.m[key]; ok {
		return b
	}

	b := connect.NewClientLimitBackoff()
	clientLimitHolds.m[key] = b

	if path, err := clientLimitHoldPath(); err == nil {
		file := make(clientLimitHoldFile)
		if oomReadJSON(path, &file) {
			if retryUnix, ok := file[key]; ok && retryUnix > now.Unix() {
				b.Restore(time.Unix(retryUnix, 0))
			}
		}
	}

	go runClientLimitHoldPersistWatcher(key, b)

	return b
}

func runClientLimitHoldPersistWatcher(clientId string, b *connect.ClientLimitBackoff) {
	for {
		st, ch := b.Get()
		_ = persistClientLimitHold(clientId, st, time.Now())
		<-ch
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

	for clientId, b := range clientLimitHolds.m {
		if retryUnix, ok := file[clientId]; ok && retryUnix > now.Unix() {
			b.Restore(time.Unix(retryUnix, 0))
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
	for id, b := range clientLimitHolds.m {
		snaps = append(snaps, snapshot{id: id, st: b.Status()})
	}
	clientLimitHolds.Unlock()

	now := time.Now()
	for _, snap := range snaps {
		_ = persistClientLimitHold(snap.id, snap.st, now)
	}
}

func resetClientLimitHoldsForTest() {
	clientLimitHolds.Lock()
	defer clientLimitHolds.Unlock()
	clientLimitHolds.m = make(map[string]*connect.ClientLimitBackoff)
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
