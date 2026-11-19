package connect

import "errors"

// ErrExtenderPacketCarrierUnavailable indicates that no proxy packet path exists.
var ErrExtenderPacketCarrierUnavailable = errors.New("proxy packet carrier unavailable")
