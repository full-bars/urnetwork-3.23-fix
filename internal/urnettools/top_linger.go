package urnettools

import "time"

type topLingerReading struct {
	User      string
	Enabled   bool
	CheckedAt time.Time
}

// collectTopLinger reads each applicable account once. Failed checks stay unknown.
func collectTopLinger(providers []Provider, now time.Time, applicable func(Provider) bool, check func(string) (bool, error)) []topLingerReading {
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
		}
	}
	return readings
}
