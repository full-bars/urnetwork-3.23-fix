package urnettools

import "time"

type topLingerReading struct {
	User      string
	Enabled   bool
	CheckedAt time.Time
}

// collectTopLinger reads each applicable account once and merges the answers
// with the readings top already holds. A failed check keeps the last known
// reading, so a lost loginctl never hides the banner, and a check that
// succeeds replaces it. An account that is no longer discovered is dropped.
func collectTopLinger(providers []Provider, prev []topLingerReading, now time.Time, applicable func(Provider) bool, check func(string) (bool, error)) []topLingerReading {
	previous := make(map[string]topLingerReading, len(prev))
	for _, r := range prev {
		previous[r.User] = r
	}
	seen := make(map[string]bool)
	var readings []topLingerReading
	for _, p := range providers {
		if seen[p.User] || !applicable(p) {
			continue
		}
		seen[p.User] = true
		enabled, err := check(p.User)
		if err == nil {
			readings = append(readings, topLingerReading{User: p.User, Enabled: enabled, CheckedAt: now})
			continue
		}
		// The check failed, so what top already knows is still the best
		// answer. An account with no reading yet stays unknown.
		if last, ok := previous[p.User]; ok {
			readings = append(readings, last)
		}
	}
	return readings
}
