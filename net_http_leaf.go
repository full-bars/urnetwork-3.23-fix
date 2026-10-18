package connect

// Leaf additions extracted from upstream net_http.go at 20b29f79 for the
// udp-candidates port, in fork-adapted (inert) form: the fork's dialer set is
// walked via the exposed HttpClient instead of upstream's dialer.Close(). No
// production caller changes; merge when the L3 transport core lands.

import (
	"context"
)

// CloseIdleConnections closes every dialer's pooled HTTP connections and the
// internal DoH resolver's, keeping the strategy usable.
func (self *ClientStrategy) CloseIdleConnections() {
	self.mutex.Lock()
	for dialer := range self.dialers {
		if client := dialer.HttpClient(); client != nil {
			client.CloseIdleConnections()
		}
	}
	self.mutex.Unlock()
	if self.internalDohResolver != nil {
		self.internalDohResolver.CloseIdleConnections()
	}
}

// Close releases the strategy's pooled connections and its internal DoH
// resolver. The fork's strategy carries no cancel or listener-unsubscribe
// fields yet, so this closes what exists.
func (self *ClientStrategy) Close() {
	self.CloseIdleConnections()
	if self.internalDohResolver != nil {
		self.internalDohResolver.Close()
	}
}

// DohSettings returns the strategy's current DoH settings (ported from
// upstream net_http.go at 20b29f79).
func (self *ClientStrategy) DohSettings() *DohSettings {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.dohSettings
}

// Builds a direct-only strategy from the settings this one was built with
// (NewDirectClientStrategy, unpinned): no extender, VLESS server or proxy, so
// a request reaches its destination from this host's own address. It takes the
// DoH settings in force here, which a user's bootstrap DoH servers replace on
// a running strategy (`SetInternalDohSettings`). The caller closes it.
//
// The extender client's direct reads -- the operator hint and its country
// (net_extender_proximity.go, GetExtenderHint) -- build one per read: a
// whitelist-only network answers no operator address through this host's own
// path, and a read over it must not take the extender path it is trying to
// place. Ported from the pin's net_http.go; this fork's strategy has no
// baseSettings field, so the settings it was built with are the ones copied.
func (self *ClientStrategy) newDirectClientStrategy(ctx context.Context) *ClientStrategy {
	settings := *self.settings
	settings.DohSettings = self.DohSettings()
	return NewDirectClientStrategy(ctx, &settings, 0)
}

// SetInternalDohSettings replaces the DoH settings of a running strategy: the
// internal DoH cache its protected control names resolve through is rebuilt
// from them (ported from upstream net_http.go at 20b29f79).
func (self *ClientStrategy) SetInternalDohSettings(dohSettings *DohSettings) {
	if dohSettings == nil {
		dohSettings = DefaultDohSettings()
	}
	replacedCache := func() *DohCache {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		self.dohSettings = dohSettings
		if self.internalDohResolver == nil {
			return nil
		}
		return self.internalDohResolver.replaceCache(NewDohCache(internalDohSettings(dohSettings)))
	}()
	// closed outside the lock: closing joins the requests in flight on it
	if replacedCache != nil {
		replacedCache.Close()
	}
}
