package main

import (
	"github.com/urnetwork/connect"
)

// desiredProxySet is the proxy set a reload acts on: the primary source (the
// --proxy_file, or the internal config when none is set) with the URL cache
// merged in. reload() and `proxy refresh` both build it through
// resolveDesiredProxySet, so the removals the preview lists are the removals
// the reload performs. Keys are proxy identities (ProxySettings.Key()).
type desiredProxySet struct {
	set      map[string]*connect.ProxySettings
	sourceOf map[string]string
	// urlCacheLoaded is false when proxy_url.json could not be read. The set
	// then lacks every URL-sourced proxy, so anything URL-sourced must be
	// treated as unknown, not as removed (see urlProxyDesiredUnknown).
	urlCacheLoaded bool
	// anySourceConfigured is true when a primary file or at least one URL
	// source is configured. It tells a configured-but-empty source apart from
	// a deliberate direct-only node.
	anySourceConfigured bool
}

// resolveDesiredProxySet reads the primary source and merges the URL cache.
// sourcePath is the --proxy_file recorded in proxy.state ("" means the
// internal config). The only error is an unreadable primary file: the caller
// must skip, never diff against zero proxies, or a transient read error would
// cancel the whole fleet. An unreadable proxy_url.json is not an error here;
// it is reported through urlCacheLoaded.
func resolveDesiredProxySet(sourcePath string) (*desiredProxySet, error) {
	var desired []*connect.ProxySettings
	if sourcePath != "" {
		settings, err := readProxySettingsFromFile(sourcePath)
		if err != nil {
			return nil, err
		}
		desired = settings
	} else {
		desired = readProxySettings()
	}

	// Keyed by identity, not bare address: two accounts at one gateway address
	// must not collide. See reload() for the incident behind that rule.
	d := &desiredProxySet{
		set:                 make(map[string]*connect.ProxySettings, len(desired)),
		sourceOf:            make(map[string]string, len(desired)),
		urlCacheLoaded:      true,
		anySourceConfigured: sourcePath != "",
	}
	primarySource := "internal"
	if sourcePath != "" {
		primarySource = "file"
	}
	for _, s := range desired {
		d.set[s.Key()] = s
		d.sourceOf[s.Key()] = primarySource
	}

	if urlState, err := readProxyURLState(); err != nil {
		tlog("[proxy][url] warning: could not read proxy_url.json: %v\n", err)
		d.urlCacheLoaded = false
	} else {
		if len(urlState.Sources) > 0 {
			d.anySourceConfigured = true
		}
		mergeProxyURLCache(d.set, d.sourceOf, urlState)
	}
	return d, nil
}

// values returns the desired settings in an unspecified order.
func (d *desiredProxySet) values() []*connect.ProxySettings {
	out := make([]*connect.ProxySettings, 0, len(d.set))
	for _, s := range d.set {
		out = append(out, s)
	}
	return out
}

// urlProxyDesiredUnknown reports whether a tracked proxy's desired state is
// UNKNOWN because proxy_url.json was unreadable: URL-sourced, or with no
// recorded source (state written before sources were recorded). Such a
// proxy is left alone; a file or internal proxy is not, because the operator
// removed it from a source the reload did read.
func urlProxyDesiredUnknown(proxies map[string]ProxyEntry, key string) bool {
	e, ok := proxies[key]
	return !ok || e.Source == "" || e.Source == "url"
}

// planProxyRefreshFromState is what `proxy refresh` previews: the diff of
// proxy.state against the desired set reload() would build, with the same
// guard for an unreadable URL cache. It writes nothing.
func planProxyRefreshFromState(state *ProxyState) (added []string, removed []refreshRemoval, err error) {
	d, err := resolveDesiredProxySet(state.Source)
	if err != nil {
		return nil, nil, err
	}
	added, removed = planProxyRefresh(d.values(), state.Proxies)
	if !d.urlCacheLoaded {
		kept := removed[:0]
		for _, rp := range removed {
			if urlProxyDesiredUnknown(state.Proxies, rp.key) {
				continue
			}
			kept = append(kept, rp)
		}
		removed = kept
	}
	return added, removed, nil
}
