package connect

import (
	"errors"
	"fmt"
	"time"
)

// A contract that arrives this far past its signed deadline is more likely a
// wrong local clock than a slow network: the deadline is the platform's wall
// clock and the comparison is ours.
const contractExpiredClockSkewSuspect = 5 * time.Minute

var contractClockSkewLogThrottle = newLogThrottle(10 * time.Minute)

var errContractExpired = errors.New("contract expired")

// pure, clock passed in; nil = legacy, no limit; equality is expired
func contractExpiredAt(deadline *int64, nowUnixMilli int64) bool {
	return deadline != nil && *deadline <= nowUnixMilli
}

func (self *sequenceContract) expired() bool {
	return contractExpiredAt(self.expirationTimeUnixMilli, time.Now().UnixMilli())
}

// How far past its deadline the contract is at now; zero when unexpired or
// without a deadline.
func contractExpiredBy(deadline *int64, nowUnixMilli int64) time.Duration {
	if !contractExpiredAt(deadline, nowUnixMilli) {
		return 0
	}
	return time.Duration(nowUnixMilli-*deadline) * time.Millisecond
}

// errContractExpired carrying how late the contract was, so the receive sequence
// can tell an ordinary expiry from a skewed clock.
func (self *sequenceContract) expiredError() error {
	late := contractExpiredBy(self.expirationTimeUnixMilli, time.Now().UnixMilli())
	return fmt.Errorf("%w %s past its deadline", errContractExpired, late.Round(time.Millisecond))
}

func (self *sequenceContract) expiredClockSkewSuspect() bool {
	return contractExpiredClockSkewSuspect <= contractExpiredBy(self.expirationTimeUnixMilli, time.Now().UnixMilli())
}
