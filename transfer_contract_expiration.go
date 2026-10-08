package connect

import (
	"errors"
	"time"
)

var errContractExpired = errors.New("contract expired")

// pure, clock passed in; nil = legacy, no limit; equality is expired
func contractExpiredAt(deadline *int64, nowUnixMilli int64) bool {
	return deadline != nil && *deadline <= nowUnixMilli
}

func (self *sequenceContract) expired() bool {
	return contractExpiredAt(self.expirationTimeUnixMilli, time.Now().UnixMilli())
}
