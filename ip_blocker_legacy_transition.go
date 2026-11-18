package connect

import (
	"encoding/binary"
	"fmt"
)

// legacyBlockerBuckets is a temporary adapter for the old flat table. Remove
// it when the paired upstream bucket tables are installed.
func legacyBlockerBuckets(data string, count int) (string, string, error) {
	if count <= 0 || len(data)%blockerHostRecordLen != 0 || len(data)/blockerHostRecordLen != count {
		return "", "", fmt.Errorf("invalid legacy blocker table length")
	}
	index := make([]byte, blockerHostBucketBytes)
	tails := make([]byte, 0, count*blockerHostTailLen)
	record := 0
	for bucket := 0; bucket < 256; bucket++ {
		binary.BigEndian.PutUint32(index[bucket*4:], uint32(record))
		for record < count && int(data[record*blockerHostRecordLen]) == bucket {
			i := record * blockerHostRecordLen
			tails = append(tails, data[i+1:i+blockerHostRecordLen]...)
			record++
		}
	}
	binary.BigEndian.PutUint32(index[256*4:], uint32(record))
	if record != count {
		return "", "", fmt.Errorf("legacy blocker buckets not sorted")
	}
	return string(index), string(tails), nil
}
