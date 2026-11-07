package connect

// Support for net_extender_activate.go, taken unchanged from the modern connect
// tree so the activation loop builds here without syncing the surrounding files.

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// uniformJitter is one draw uniform in [0, maxJitter), none when maxJitter is
// non-positive. A non-nil testDraw replaces the draw, and its result is held
// to the same range, so a test can pick any point of the window but never
// leave it.
func uniformJitter(maxJitter time.Duration, testDraw func(maxJitter time.Duration) time.Duration) time.Duration {
	if maxJitter <= 0 {
		return 0
	}
	if testDraw != nil {
		return max(0, min(maxJitter-1, testDraw(maxJitter)))
	}
	return time.Duration(rand.Int63n(int64(maxJitter)))
}

// The error of a failed request as a log line shows it. An exhausted request's
// own message is fixed and names no cause, so the causes its attempts retained
// follow it, which is what shows the failing candidate in the field
// (connect#229); a cause the retries repeated is named once, with its count.
// The causes are the leaves the exhaustion already unwrapped -- a dial, tls,
// quic or dns error, a deadline -- so neither the request url, which may carry
// an env secret, nor the headers, which carry the jwt, reach the line: a url
// error kept whole for want of a cause is reduced to its operation. Any other
// error is its own message.
func httpRequestErrorWithCauses(err error) string {
	if err == nil {
		return ""
	}
	var exhausted *HttpRequestExhaustedError
	if !errors.As(err, &exhausted) || exhausted == nil {
		return err.Error()
	}
	causeMessages := []string{}
	causeMessageCounts := map[string]int{}
	for _, cause := range exhausted.Unwrap() {
		var causeMessage string
		if urlError, ok := cause.(*url.Error); ok {
			causeMessage = fmt.Sprintf("%s: %v", urlError.Op, urlError.Err)
		} else {
			causeMessage = cause.Error()
		}
		if causeMessageCounts[causeMessage] == 0 {
			causeMessages = append(causeMessages, causeMessage)
		}
		causeMessageCounts[causeMessage] += 1
	}
	if len(causeMessages) == 0 {
		return err.Error()
	}
	for i, causeMessage := range causeMessages {
		if count := causeMessageCounts[causeMessage]; 1 < count {
			causeMessages[i] = fmt.Sprintf("%s (x%d)", causeMessage, count)
		}
	}
	return fmt.Sprintf("%s: %s", err.Error(), strings.Join(causeMessages, "; "))
}

// The hello fields a probe reads. The server's HelloResult carries more, and
// an unknown field is ignored, so this stays valid as hello grows.
type extenderHelloResult struct {
	ClientAddress string `json:"client_address"`
}

// Whether the newest record held for `publicKey` is valid -- neither revoked
// nor expired (B5) -- and names an address of `ipVersion` that is still this
// key's. Only the record the operator signed last says what it publishes now:
// the addresses older records of the key listed, which the directory keeps
// (extenderDirectoryRecord.ips), do not count, nor does an address another key
// has claimed since. Local dial evidence does not enter, so a held address
// counts. The extender activation loop reads it for its own key (G3).
func (self *ExtenderDirectory) CurrentRecordNamesFamily(publicKey []byte, ipVersion int) bool {
	keyHex := hex.EncodeToString(publicKey)
	now := self.settings.Now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.rootKeySet.Len() == 0 {
		return false
	}
	keyRecord := self.keyHexRecords[keyHex]
	if keyRecord == nil || keyRecord.recordBody == nil {
		return false
	}
	if self.keyRecordRevokedWithLock(keyRecord) || self.keyRecordExpiredWithLock(keyRecord, now) {
		return false
	}
	for _, recordAddress := range keyRecord.recordBody.Addresses {
		ip, err := netip.ParseAddr(recordAddress.Ip)
		if err != nil || !ip.IsValid() {
			continue
		}
		ip = ip.Unmap()
		if addressIpVersion(ip) != ipVersion {
			continue
		}
		if address := self.ipAddresses[ip]; address != nil && address.publicKeyHex == keyHex {
			return true
		}
	}
	return false
}
