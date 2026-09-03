package connect

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	mathrand "math/rand"
	"testing"

	"github.com/go-playground/assert/v2"
)

func TestMessagePool(t *testing.T) {
	ResetMessagePoolStats()
	for n := range 1024 {
		if n%32 == 0 {
			fmt.Printf("mem[%d]\n", n)
		}
		for range 16 {
			message := make([]byte, n)
			mathrand.Read(message)

			messageCopy := MessagePoolCopy(message)
			assert.Equal(t, len(messageCopy), n)
			assert.Equal(t, message, messageCopy)

			MessagePoolReturn(messageCopy)
		}
	}
	for n := range 1024 {
		if n%32 == 0 {
			fmt.Printf("memr[%d]\n", n)
		}
		b := make([]byte, mathrand.Intn(32*1024))
		mathrand.Read(b)
		bCopy, err := MessagePoolReadAll(bytes.NewReader(b))
		assert.Equal(t, err, nil)
		assert.Equal(t, b, bCopy)
		MessagePoolReturn(bCopy)
	}
	stats := MessagePoolStats()
	for _, tagRatios := range stats {
		for _, ratio := range tagRatios {
			assert.Equal(t, ratio, float32(1.0))
		}
	}
}

func TestMessagePoolShare(t *testing.T) {
	holdCount := 16
	holdMessages := make([][][]byte, holdCount)

	for range 1024 {
		message := MessagePoolGet(mathrand.Intn(4096))
		pooled, shared := MessagePoolCheck(message)
		assert.Equal(t, pooled, true)
		assert.Equal(t, shared, false)
		holdMessages[0] = append(holdMessages[0], message)
		k := mathrand.Intn(holdCount)
		for i := 1; i < k; i += 1 {
			MessagePoolShareReadOnly(message)
			pooled, shared = MessagePoolCheck(message)
			assert.Equal(t, pooled, true)
			assert.Equal(t, shared, true)
			holdMessages[i] = append(holdMessages[i], message)
		}
	}

	// add large messages, spanning both the pooled and non-pooled size ranges.
	// messages larger than the largest pool bucket are not pooled. This fork
	// added buckets above the original 4096 maximum, so derive the threshold
	// from the live pool configuration rather than hardcoding it (otherwise the
	// test goes stale whenever the bucket sizes change).
	pools := orderedMessagePools()
	maxPoolSize := pools[len(pools)-1].size
	for range 1024 {
		message := MessagePoolGet(mathrand.Intn(2 * maxPoolSize))
		pooled, shared := MessagePoolCheck(message)
		assert.Equal(t, pooled, len(message) <= maxPoolSize)
		assert.Equal(t, shared, false)
		k := mathrand.Intn(holdCount)
		for i := 1; i < k; i += 1 {
			MessagePoolShareReadOnly(message)
			pooled, shared = MessagePoolCheck(message)
			assert.Equal(t, pooled, len(message) <= maxPoolSize)
			assert.Equal(t, shared, len(message) <= maxPoolSize)
		}
		for i := 1; i < k; i += 1 {
			MessagePoolReturn(message)
			pooled, shared = MessagePoolCheck(message)
			assert.Equal(t, pooled, len(message) <= maxPoolSize)
			assert.Equal(t, shared, len(message) <= maxPoolSize)
		}
		MessagePoolReturn(message)
		pooled, shared = MessagePoolCheck(message)
		assert.Equal(t, pooled, false)
		assert.Equal(t, shared, false)
	}

	for i := holdCount - 1; 1 <= i; i -= 1 {
		for _, message := range holdMessages[i] {
			pooled, shared := MessagePoolCheck(message)
			assert.Equal(t, pooled, len(message) <= 4096)
			assert.Equal(t, shared, len(message) <= 4096)
			r := MessagePoolReturn(message)
			assert.Equal(t, r, false)
		}
	}
	for _, message := range holdMessages[0] {
		r := MessagePoolReturn(message)
		assert.Equal(t, r, true)
		pooled, shared := MessagePoolCheck(message)
		assert.Equal(t, pooled, false)
		assert.Equal(t, shared, false)
	}
}

func TestMessagePoolShardRouting(t *testing.T) {
	pool := newMessagePool(2048, 32)
	defer pool.Clear()

	m := pool.Get()
	full := m[:cap(m)]
	shardIndex := int(full[pool.size+12])
	assert.Equal(t, shardIndex >= 0, true)
	assert.Equal(t, shardIndex < pool.shardCount, true)

	shard := pool.shard(shardIndex)
	before := shard.count
	pool.Put(m)
	assert.Equal(t, shard.count, before+1)

	m2 := pool.Get()
	full2 := m2[:cap(m2)]
	shardIndex2 := int(full2[pool.size+12])
	assert.Equal(t, shardIndex2 >= 0, true)
	assert.Equal(t, shardIndex2 < pool.shardCount, true)

	shard2 := pool.shard(shardIndex2)
	before2 := shard2.count
	pool.Put(m2)
	assert.Equal(t, shard2.count, before2+1)
}

func TestMessagePoolShardWithTag(t *testing.T) {
	pools := orderedMessagePools()
	pool := pools[0]

	m, _ := MessagePoolGetDetailedWithTag(pool.size, 42)
	full := m[:cap(m)]

	shardIndex := int(full[pool.size+12])

	r := MessagePoolReturn(m)
	assert.Equal(t, r, true)

	shard := pool.shard(shardIndex)
	assert.Equal(t, shard.returnedTags[42], uint64(1))
}

func TestMessagePoolShardRoundRobin(t *testing.T) {
	pool := newMessagePool(2048, 1024)
	defer pool.Clear()

	shardHits := make([]int, pool.shardCount)
	for range pool.shardCount * 100 {
		m := pool.Get()
		si := int(m[pool.size+12])
		shardHits[si]++
		pool.Put(m)
	}

	for i, hits := range shardHits {
		assert.Equal(t, hits > 0, true)
		t.Logf("shard[%d] = %d hits", i, hits)
	}
}

func TestMessagePoolShardContention(t *testing.T) {
	const goroutines = 32
	const iterations = 1000
	const dataSize = 1500

	done := make(chan bool)

	for range goroutines {
		go func() {
			for range iterations {
				m := MessagePoolGet(dataSize)
				m[0] = byte('x')
				m[dataSize-1] = byte('y')
				MessagePoolReturn(m)
			}
			done <- true
		}()
	}

	for range goroutines {
		<-done
	}

	t.Log("contention test completed")
}

func TestMessagePoolShardTagConcurrent(t *testing.T) {
	const goroutines = 16
	const iterations = 500

	done := make(chan bool)

	for i := range goroutines {
		tag := uint8(i + 1)
		go func() {
			for range iterations {
				m, _ := MessagePoolGetDetailedWithTag(1500, tag)
				m[0] = byte('x')
				MessagePoolReturn(m)
			}
			done <- true
		}()
	}

	for range goroutines {
		<-done
	}

	pool := orderedMessagePools()[0]
	for i := range goroutines {
		tag := uint8(i + 1)
		var taken, returned, created uint64
		for _, shard := range pool.shards {
			shard.mutex.Lock()
			taken += shard.takenTags[tag]
			returned += shard.returnedTags[tag]
			created += shard.createdTags[tag]
			shard.mutex.Unlock()
		}
		assert.Equal(t, taken, uint64(iterations))
		assert.Equal(t, returned, uint64(iterations))
		t.Logf("tag[%d] taken=%d returned=%d created=%d", tag, taken, returned, created)
	}
}

func TestMessagePoolShardPowerOfTwo(t *testing.T) {
	powersOfTwo := []int{1, 2, 4, 8, 16, 32, 64, 128, 256}
	for _, v := range powersOfTwo {
		assert.Equal(t, v&(v-1) == 0, true)
	}
	nonPowersOfTwo := []int{3, 5, 6, 7, 9, 10, 15, 31, 63, 127, 255}
	for _, v := range nonPowersOfTwo {
		assert.Equal(t, v&(v-1) == 0, false)
	}
}

// TestMessagePoolRefusedShareFlagsNoReturn is the L4 regression: a share
// refused at refcount overflow (uint16 max) must not let a later
// MessagePoolReturn on the refused share over-decrement a legitimate owner's
// reference. Before the fix, the refusal was only a log line — the caller
// held an indistinguishable buffer and a stray Return would decrement the
// count exactly as if the share had succeeded, corrupting the real owners'
// accounting.
func TestMessagePoolRefusedShareFlagsNoReturn(t *testing.T) {
	message := MessagePoolGet(64)
	c := cap(message)
	poolSize := c - MessagePoolMetaByteCount
	poolMessage := message[:c]

	// Simulate a buffer already at max refcount (65535 concurrent read-only
	// shares) rather than looping that many times.
	binary.BigEndian.PutUint16(poolMessage[poolSize+10:], ^uint16(0))

	refusedShareCount := func() uint8 {
		return (poolMessage[poolSize+9] & MessagePoolRefusedShareCountMask) >> MessagePoolRefusedShareCountShift
	}

	shared := MessagePoolShareReadOnly(message)
	if &shared[0] != &message[0] {
		t.Fatal("MessagePoolShareReadOnly returned a different buffer on refusal")
	}
	if refusedShareCount() != 1 {
		t.Fatalf("refused share did not set refused-share count to 1, got %d", refusedShareCount())
	}
	if count := binary.BigEndian.Uint16(poolMessage[poolSize+10:]); count != ^uint16(0) {
		t.Fatalf("refused share mutated the refcount: got %d, want unchanged max", count)
	}

	// A caller that (incorrectly, per the refusal warning) returns the
	// refused share anyway must be a no-op — NOT a decrement of the count
	// that legitimate owners are still relying on.
	MessagePoolReturn(message)
	if count := binary.BigEndian.Uint16(poolMessage[poolSize+10:]); count != ^uint16(0) {
		t.Fatalf("no-return count did not prevent decrement: count = %d, want unchanged max", count)
	}
	if refusedShareCount() != 0 {
		t.Fatalf("refused-share count not decremented after being consumed by Return, got %d", refusedShareCount())
	}

	// A single refusal is forgiven by exactly one no-op Return: the next
	// Return decrements normally like any legitimate owner's Return.
	MessagePoolReturn(message)
	if count := binary.BigEndian.Uint16(poolMessage[poolSize+10:]); count != ^uint16(0)-1 {
		t.Fatalf("post-forgiveness return did not decrement: count = %d, want %d", count, ^uint16(0)-1)
	}
}

// TestMessagePoolStackedRefusedSharesEachGetForgiveness is the regression
// for the CodeRabbit finding on the original single-bit fix: two
// MessagePoolShareReadOnly calls can both be refused while the count is
// saturated at max. A single bit can only record "refused at least once",
// so the first Return would clear it and the SECOND Return would fall
// through to a real decrement — over-releasing a legitimate owner's
// reference by one. The counter must instead require one no-op Return per
// refusal before decrementing resumes.
func TestMessagePoolStackedRefusedSharesEachGetForgiveness(t *testing.T) {
	message := MessagePoolGet(64)
	c := cap(message)
	poolSize := c - MessagePoolMetaByteCount
	poolMessage := message[:c]

	binary.BigEndian.PutUint16(poolMessage[poolSize+10:], ^uint16(0))

	refusedShareCount := func() uint8 {
		return (poolMessage[poolSize+9] & MessagePoolRefusedShareCountMask) >> MessagePoolRefusedShareCountShift
	}

	// Two independent callers both attempt to share while refcount is
	// saturated; both are refused.
	MessagePoolShareReadOnly(message)
	MessagePoolShareReadOnly(message)
	if refusedShareCount() != 2 {
		t.Fatalf("two stacked refusals: got refused-share count %d, want 2", refusedShareCount())
	}

	// First no-op return: forgives exactly one refusal, must NOT decrement
	// the real refcount.
	MessagePoolReturn(message)
	if count := binary.BigEndian.Uint16(poolMessage[poolSize+10:]); count != ^uint16(0) {
		t.Fatalf("first return after stacked refusals decremented early: count = %d, want unchanged max", count)
	}
	if refusedShareCount() != 1 {
		t.Fatalf("first return did not consume exactly one refusal, got refused-share count %d", refusedShareCount())
	}

	// Second no-op return: forgives the second refusal, still must NOT
	// decrement — this is exactly the case the single-bit version got wrong.
	MessagePoolReturn(message)
	if count := binary.BigEndian.Uint16(poolMessage[poolSize+10:]); count != ^uint16(0) {
		t.Fatalf("second return after stacked refusals decremented (single-bit regression): count = %d, want unchanged max", count)
	}
	if refusedShareCount() != 0 {
		t.Fatalf("second return did not clear refused-share count, got %d", refusedShareCount())
	}

	// Both refusals now forgiven: the next Return is a legitimate owner's
	// Return and must decrement for real.
	MessagePoolReturn(message)
	if count := binary.BigEndian.Uint16(poolMessage[poolSize+10:]); count != ^uint16(0)-1 {
		t.Fatalf("post-forgiveness return did not decrement: count = %d, want %d", count, ^uint16(0)-1)
	}
}

func TestBase64(t *testing.T) {
	for range 128 {
		n := mathrand.Intn(512)
		b := make([]byte, n)
		mathrand.Read(b)
		b2, err := DecodeBase64(base64.StdEncoding, EncodeBase64(base64.StdEncoding, b))
		assert.Equal(t, err, nil)
		assert.Equal(t, b, b2)
	}
}
