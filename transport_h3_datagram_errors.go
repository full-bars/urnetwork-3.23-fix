package connect

import "errors"

// errQuicDatagramFlightFull is what upstream's QUIC send path returns when its
// retained DATAGRAM flight is full. The datagram layer treats it as local
// pressure and moves the message to the reliable stream. This fork has no such
// send path yet, so nothing produces it, but it is declared so the layer stays
// identical to upstream and picks the behaviour up the day a producer exists.
var errQuicDatagramFlightFull = errors.New("QUIC retained datagram flight is full")
