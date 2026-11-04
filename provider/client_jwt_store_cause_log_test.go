package main

import (
	"time"
)

func fixedEntry(jwt string) clientJWTEntry {
	return clientJWTEntry{
		ByClientJWT: jwt,
		ClientID:    testClientId,
		NetworkID:   "net-main",
		MintedAt:    time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
	}
}
