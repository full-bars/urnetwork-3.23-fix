package main

// proxyGradeInfo is the grade payload attached to a proxy report row. The
// hub mirrors these fields so a dashboard can render the provider's A-F
// tier. Only populated when a proxy has been graded (Graded=true); an
// ungraded proxy simply omits the fields (omitempty), keeping the report
// payload backward-compatible for older hubs.
type proxyGradeInfo struct {
	Score      float64  `json:"score,omitempty"`
	Graded     bool     `json:"graded,omitempty"`
	Failed     []string `json:"failed,omitempty"`
	Tier       string   `json:"tier,omitempty"`
	LastGraded int64    `json:"last_graded,omitempty"` // unix ts, 0 = never
}

// proxyGradeFor resolves the A-F grade for an address from BOTH grade
// stores. Precedence: the paid/file store (proxy.state, ProxyEntry) wins
// over the URL store (proxy_url.json, ProxyURLEntry) when both are graded —
// a paid proxy that also appears in a free URL list is owned, so the
// deliberate config grade is the meaningful one. ok=false when neither
// store has a graded entry for the address.
func proxyGradeFor(addr string, paid *ProxyState, url *ProxyURLState) (proxyGradeInfo, bool) {
	if paid != nil {
		if entry, ok := paid.Proxies[addr]; ok && entry.Graded {
			info := proxyGradeInfo{
				Score:  entry.Score,
				Graded: true,
				Failed: entry.Failed,
				Tier:   proxyGradeTier(entry.Score),
			}
			if !entry.LastGraded.IsZero() {
				info.LastGraded = entry.LastGraded.Unix()
			}
			return info, true
		}
	}
	if url != nil {
		if entry, ok := url.Cache[addr]; ok && entry.Graded {
			info := proxyGradeInfo{
				Score:  entry.Score,
				Graded: true,
				Failed: entry.Failed,
				Tier:   proxyGradeTier(entry.Score),
			}
			if !entry.LastGraded.IsZero() {
				info.LastGraded = entry.LastGraded.Unix()
			}
			return info, true
		}
	}
	return proxyGradeInfo{}, false
}
