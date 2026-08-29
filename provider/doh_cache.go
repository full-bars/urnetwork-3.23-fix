package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/urnetwork/connect"
)

const dohScoresFilename = ".doh_scores"
const dohScoresSaveInterval = 5 * time.Minute

// dohScoresPath returns the path to the persisted server-score file.
func dohScoresPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", dohScoresFilename), nil
}

// loadDohScores reads the previously-persisted server scores from disk.
// Returns an empty map when the file doesn't exist (first run).
func loadDohScores() (map[string]float64, error) {
	p, err := dohScoresPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]float64{}, nil
		}
		return nil, err
	}
	var scores map[string]float64
	if err := json.Unmarshal(b, &scores); err != nil {
		return nil, err
	}
	if scores == nil {
		scores = map[string]float64{}
	}
	return scores, nil
}

// saveDohScores writes the current server scores to disk as JSON,
// atomically (tmp+rename), with 0600 permissions. Creates the
// ~/.urnetwork directory if it does not exist.
func saveDohScores(scores map[string]float64) error {
	if len(scores) == 0 {
		return nil
	}
	p, err := dohScoresPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	b, err := json.Marshal(scores)
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// initPersistentDohCache creates a DohCache with persisted server scores,
// starts the warm-up probe, and spawns periodic score persistence.
// Returns the cache and a save+close function for the caller to defer.
func initPersistentDohCache(ctx context.Context) (*connect.DohCache, func()) {
	settings := connect.DefaultDohSettings()
	settings.DnsResolverSettings = connect.DefaultDnsResolverSettings()

	// Load last session's scores so the fan-out starts already biased
	// toward the servers that were fastest last session.
	if scores, err := loadDohScores(); err != nil {
		tlog("[doh] warning: could not load persisted server scores: %v\n", err)
	} else if len(scores) > 0 {
		settings.ServerStatsSeed = scores
	}

	cache := connect.NewDohCache(settings)
	connect.SetSharedDohCache(cache)
	cache.Warm()

	// Periodic persistence goroutine — saves scores every 5m so a crash
	// loses at most one window of signal.
	go func() {
		ticker := time.NewTicker(dohScoresSaveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if scores := cache.ServerScores(); len(scores) > 0 {
					if err := saveDohScores(scores); err != nil {
						tlog("[doh] warning: could not persist server scores: %v\n", err)
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Returns a deferred cleanup that saves final scores, clears the shared
	// cache reference, and shuts down in-flight queries.
	close := func() {
		if scores := cache.ServerScores(); len(scores) > 0 {
			if err := saveDohScores(scores); err != nil {
				tlog("[doh] warning: could not persist server scores on shutdown: %v\n", err)
			}
		}
		connect.SetSharedDohCache(nil)
		cache.Close()
	}

	return cache, close
}