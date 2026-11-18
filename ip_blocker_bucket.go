package connect

import "fmt"

// A bucket table indexes all 256 possible first fingerprint bytes using 257
// big-endian record offsets. The records contain the remaining five bytes.
const blockerHostTailLen = blockerHostRecordLen - 1
const blockerHostBucketBytes = 257 * 4
const blockerMaxEmbeddedBytes = 1 << 20

type blockerHostBuckets struct {
	index string
	tails string
}

// decodeBlockerHostBuckets validates the complete index before any lookup.
// Empty individual buckets are valid; an empty or truncated index is not.
func decodeBlockerHostBuckets(index, tails string, count, otherBytes int) (*blockerHostBuckets, error) {
	if count < 0 || otherBytes < 0 || otherBytes > blockerMaxEmbeddedBytes ||
		len(index) > blockerMaxEmbeddedBytes-otherBytes ||
		len(tails) > blockerMaxEmbeddedBytes-otherBytes-len(index) {
		return nil, fmt.Errorf("blocker embedded data exceeds size limit or has invalid count")
	}
	if len(index) != blockerHostBucketBytes {
		return nil, fmt.Errorf("blocker bucket index length %d, want %d", len(index), blockerHostBucketBytes)
	}
	if len(tails)%blockerHostTailLen != 0 || len(tails)/blockerHostTailLen != count {
		return nil, fmt.Errorf("blocker tail length %d does not match count %d", len(tails), count)
	}
	previous := uint64(0)
	for i := 0; i <= 256; i++ {
		offset := beN(index, i*4, 4)
		if (i == 0 && offset != 0) || offset < previous || offset > uint64(count) || (i == 256 && offset != uint64(count)) {
			return nil, fmt.Errorf("blocker bucket offset %d is invalid", i)
		}
		if i > 0 {
			for record := previous + 1; record < offset; record++ {
				if beN(tails, int(record-1)*blockerHostTailLen, blockerHostTailLen) >= beN(tails, int(record)*blockerHostTailLen, blockerHostTailLen) {
					return nil, fmt.Errorf("blocker bucket %d records are not strictly sorted", i-1)
				}
			}
		}
		previous = offset
	}
	return &blockerHostBuckets{index: index, tails: tails}, nil
}

// newBucketBlockerWithData rejects malformed data rather than constructing a
// blocker with incomplete protection. The caller must handle the error.
func newBucketBlockerWithData(pepper, index, tails string, count int, prefixData string, prefixCount int, prefix6Data string, prefix6Count int) (*dataBlocker, error) {
	if len(pepper) != blockerPepperLen {
		return nil, fmt.Errorf("invalid blocker pepper length")
	}
	if prefixCount < 0 || len(prefixData)%8 != 0 || len(prefixData)/8 != prefixCount {
		return nil, fmt.Errorf("invalid blocker IPv4 data length")
	}
	if prefix6Count < 0 || len(prefix6Data)%32 != 0 || len(prefix6Data)/32 != prefix6Count {
		return nil, fmt.Errorf("invalid blocker IPv6 data length")
	}
	otherBytes := len(pepper) + len(prefixData) + len(prefix6Data)
	table, err := decodeBlockerHostBuckets(index, tails, count, otherBytes)
	if err != nil {
		return nil, err
	}
	return &dataBlocker{pepper: pepper, hostCount: count, hostBuckets: table,
		prefixData: prefixData, prefixCount: prefixCount,
		prefix6Data: prefix6Data, prefix6Count: prefix6Count}, nil
}

func (table *blockerHostBuckets) contains(key uint64) bool {
	bucket := int(key >> (8 * blockerHostTailLen))
	if bucket >= 256 {
		return false
	}
	start := int(beN(table.index, bucket*4, 4))
	end := int(beN(table.index, (bucket+1)*4, 4))
	tail := key & ((1 << (8 * blockerHostTailLen)) - 1)
	return searchHash(table.tails[start*blockerHostTailLen:end*blockerHostTailLen], end-start, blockerHostTailLen, tail)
}
