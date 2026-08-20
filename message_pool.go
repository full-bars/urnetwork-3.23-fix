package connect

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/maphash"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/exp/maps"

	"google.golang.org/protobuf/proto"
)

var DefaultMessagePoolShardCount = func() int {
	if s := os.Getenv("URNETWORK_MESSAGE_POOL_SHARD_COUNT"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= 256 {
			if n&(n-1) == 0 {
				return n
			}
		}
	}
	return 16
}()

// Enhanced metrics for message pool performance tracking
type PoolMetrics struct {
	Hits             atomic.Uint64
	Misses           atomic.Uint64
	Returns          atomic.Uint64
	ActiveBuffers    atomic.Uint64
	TotalCapacity    atomic.Uint64
	ReturnLatency    atomic.Uint64 // nanoseconds
	ReturnCount      atomic.Uint64
	GCPauses         atomic.Uint64
	SizeDistribution map[int]*atomic.Uint64
	LastResetTime    time.Time
}

var globalPoolMetrics = &PoolMetrics{
	SizeDistribution: map[int]*atomic.Uint64{
		2048:  &atomic.Uint64{},
		4096:  &atomic.Uint64{},
		16384: &atomic.Uint64{},
		32768: &atomic.Uint64{},
		65536: &atomic.Uint64{},
	},
	LastResetTime: time.Now(),
}

// new byte allocations in the connect package use pooled message buffers,
// either via `MessagePoolCopy` or `MessagePoolGet`.
// There are three rules for pooled messages:
// - an owner of a message should return the message to the pool with `MessagePoolReturn`
//
//	when no longer used.
//
// - message ownership is handed off on send/channel write.
//
//	If the caller wants to retain the passed message, it should call `MessagePoolShareReadOnly`
//	before calling send/channel write.
//
// - messages are valid only for duration of a receive callback.
//
//	If the receiver wants to keep the message longer, it shoudl call `MessagePoolShareReadOnly`
//	before the callback returns.
//
// Shared messages are returned to the pool the same as normal messages.
// `MessagePoolReturn`/`MessagePoolShareReadOnly` is a noop when using a `[]byte` that is not part of the pool.

// set this to true to tag messages with useful debugging information e.g. the creation site
const debugTags = false

// [8 byte id][1 byte tag][1 byte flags][2 byte ref count][1 byte shard index]
const MessagePoolMetaByteCount = 13
const MessagePoolFlagShared = uint8(0x01)

var InitialMessagePoolByteCount = mib(2)

type poolShard struct {
	mutex        sync.Mutex
	pool         [][]byte
	count        int
	nextId       uint64
	takenTags    [256]uint64
	returnedTags [256]uint64
	createdTags  [256]uint64
}

func newPoolShard(maxCount int) *poolShard {
	return &poolShard{
		pool:  make([][]byte, maxCount),
		count: 0,
	}
}

type messagePool struct {
	size       int
	shards     []*poolShard
	shardCount int
	shardMask  uint32
	shardNext  atomic.Uint64
}

func newMessagePool(size int, maxCount int) *messagePool {
	shardCount := DefaultMessagePoolShardCount
	// Per-shard freelist capacity has a floor of 1 buffer. At the shipped
	// defaults this distributes correctly (e.g. lowmem's 1 MiB budget yields
	// 512 pool entries / 16 shards = 32 per shard). If shard count is raised
	// significantly without raising the pool budget, this floor could inflate
	// memory beyond the intended per-class cap.
	maxCountPerShard := maxCount / shardCount
	if maxCountPerShard < 1 {
		maxCountPerShard = 1
	}

	mp := &messagePool{
		size:       size,
		shards:     make([]*poolShard, shardCount),
		shardCount: shardCount,
		shardMask:  uint32(shardCount - 1),
	}
	for i := range shardCount {
		mp.shards[i] = newPoolShard(maxCountPerShard)
	}
	return mp
}

// shard returns the poolShard at index without bounds checking; indexes come
// from nextShardIndex or from the shard-index meta byte of a message buffer.
func (self *messagePool) shard(index int) *poolShard {
	return self.shards[index]
}

// nextShardIndex returns the index of the next shard for Get, rotating
// round-robin across the shards by advancing an atomic counter and masking
// it with shardMask (the shard count must be a power of two).
func (self *messagePool) nextShardIndex() int {
	return int(self.shardNext.Add(1)-1) & int(self.shardMask)
}

// Resize replaces each shard's freelist with a new slice whose per-shard
// capacity is maxCount/shardCount (floor 1), keeping the first count entries
// and discarding the rest. Buffers already handed out are not invalidated —
// the shard count and the shard-index meta byte never change, so such a
// buffer can still be Put back later and will re-enter the freelist when
// there is room.
func (self *messagePool) Resize(maxCount int) {
	maxCountPerShard := maxCount / self.shardCount
	if maxCountPerShard < 1 {
		maxCountPerShard = 1
	}
	for _, shard := range self.shards {
		shard.mutex.Lock()
		newPool := make([][]byte, maxCountPerShard)
		newCount := copy(newPool, shard.pool[:shard.count])
		shard.pool = newPool
		shard.count = newCount
		shard.mutex.Unlock()
	}
}

// Clear empties every shard's freelist (count reset to 0 and entries nil'd)
// so the pooled buffers become garbage. It does not invalidate buffers
// currently held by callers; a later Put returns them to the empty freelist.
func (self *messagePool) Clear() {
	for _, shard := range self.shards {
		shard.mutex.Lock()
		for i := range shard.count {
			shard.pool[i] = nil
		}
		shard.count = 0
		shard.mutex.Unlock()
	}
}

// the returned does not come in an initialized zero state,
// i.e. it can have garbage bytes
func (self *messagePool) Get() []byte {
	shardIndex := self.nextShardIndex()
	shard := self.shard(shardIndex)

	shard.mutex.Lock()
	defer shard.mutex.Unlock()

	if 0 < shard.count {
		poolMessage := shard.pool[shard.count-1]
		shard.pool[shard.count-1] = nil
		shard.count -= 1
		poolMessage[self.size+12] = uint8(shardIndex)
		globalPoolMetrics.Hits.Add(1)
		globalPoolMetrics.ActiveBuffers.Add(1)
		globalPoolMetrics.SizeDistribution[self.size].Add(1)
		return poolMessage
	}

	// create a new message
	poolMessage := make([]byte, self.size+MessagePoolMetaByteCount)
	shard.nextId += 1
	binary.BigEndian.PutUint64(poolMessage[self.size:], shard.nextId)
	poolMessage[self.size+8] = 255
	poolMessage[self.size+12] = uint8(shardIndex)
	globalPoolMetrics.Misses.Add(1)
	globalPoolMetrics.ActiveBuffers.Add(1)
	globalPoolMetrics.SizeDistribution[self.size].Add(1)
	return poolMessage
}

// Put returns a message that was handed out by Get back to the pool. The
// owning shard is read from the meta byte at self.size+12, so the message
// must be a full-length buffer (cap self.size+MessagePoolMetaByteCount) that
// this pool produced — Put does not verify size, ownership, or the ref
// count, and does not reset any meta bytes (MessagePoolReturn clears the
// tag/flags/ref count under the shard lock before calling Put). The message
// is pushed onto the shard's freelist when there is capacity and silently
// dropped for GC otherwise. Put works after Clear or Resize: those only
// touch the freelist, never the meta bytes of outstanding buffers.
func (self *messagePool) Put(poolMessage []byte) {
	shardIndex := int(poolMessage[self.size+12])
	if shardIndex < 0 || shardIndex >= self.shardCount {
		return
	}
	shard := self.shard(shardIndex)

	shard.mutex.Lock()
	defer shard.mutex.Unlock()

	if shard.count < len(shard.pool) {
		// note we do not need to zero out the message
		shard.pool[shard.count] = poolMessage
		shard.count += 1
		globalPoolMetrics.Returns.Add(1)
		globalPoolMetrics.ActiveBuffers.Add(^uint64(0))
	}
	// else no capacity, discard the message
}

var orderedMessagePools = sync.OnceValue(func() []*messagePool {
	pools := []*messagePool{
		newMessagePool(2048, int(InitialMessagePoolByteCount/ByteCount(2048))),
		newMessagePool(4096, int(InitialMessagePoolByteCount/ByteCount(4096))),
		newMessagePool(16384, int(InitialMessagePoolByteCount/ByteCount(16384))),
		newMessagePool(32768, int(InitialMessagePoolByteCount/ByteCount(32768))),
		newMessagePool(65536, int(InitialMessagePoolByteCount/ByteCount(65536))),
	}

	if debugTags {
		// poolStats' per-tag breakdown is only meaningful when debugTags
		// assigns real caller tags; with it off every allocation is tag 0,
		// so this would otherwise log 5 near-empty Infof lines every 60s in
		// production for no diagnostic value.
		go HandleError(func() {
			poolStats(pools)
		})
	}

	return pools
})

func poolStats(pools []*messagePool) {
	for {
		for _, pool := range pools {
			for tag := range 256 {
				var taken uint64
				var returned uint64
				var created uint64
				for _, shard := range pool.shards {
					func() {
						shard.mutex.Lock()
						defer shard.mutex.Unlock()
						taken += shard.takenTags[tag]
						returned += shard.returnedTags[tag]
						created += shard.createdTags[tag]
					}()
				}
				if 0 < taken {
					ratio := float32(returned) / float32(taken)
					reuse := float32(taken-created) / float32(taken)
					var caller string
					func() {
						debugStateLock.Lock()
						defer debugStateLock.Unlock()
						caller = strings.Join(maps.Keys(tagCallers[uint8(tag)]), "/")
					}()
					DefaultLogger().Infof("pool[%d] tag=%d [%s] r=%d/t=%d/c=%d = %.2f%% return / %.2f%% reuse\n", pool.size, tag, caller, returned, taken, created, 100*ratio, 100*reuse)
				}
			}
		}

		select {
		case <-time.After(60 * time.Second):
		}
	}
}

func ResizeMessagePools(maxByteCount ByteCount) {
	pools := orderedMessagePools()
	poolSizeCount := ByteCount(len(pools))
	for _, pool := range pools {
		pool.Resize(int(maxByteCount / poolSizeCount / ByteCount(pool.size)))
	}
}

// ResizeMessagePoolsPerClass applies the given maximum byte count directly as a ceiling
// for each message pool size class, instead of dividing it across the classes.
// This preserves the legacy buffer allocation semantics required by the provider.
func ResizeMessagePoolsPerClass(maxByteCount ByteCount) {
	pools := orderedMessagePools()
	for _, pool := range pools {
		pool.Resize(int(maxByteCount / ByteCount(pool.size)))
	}
}

func ClearMessagePools() {
	for _, pool := range orderedMessagePools() {
		pool.Clear()
	}
}

var seed = maphash.MakeSeed()
var debugStateLock sync.Mutex
var tagCallers = map[uint8]map[string]bool{}

func debugTag() uint8 {
	_, file2, line2, ok := runtime.Caller(2)
	if !ok {
		return 0
	}
	_, file3, line3, ok := runtime.Caller(3)
	if !ok {
		return 0
	}
	caller := fmt.Sprintf("%s:%d->%s:%d", file3, line3, file2, line2)
	tag := uint8(maphash.String(seed, caller))
	func() {
		debugStateLock.Lock()
		defer debugStateLock.Unlock()

		callers, ok := tagCallers[tag]
		if !ok {
			callers = map[string]bool{}
			tagCallers[tag] = callers
		}
		callers[caller] = true
	}()
	return tag
}

func ResetMessagePoolStats() {
	for _, pool := range orderedMessagePools() {
		for _, shard := range pool.shards {
			func() {
				shard.mutex.Lock()
				defer shard.mutex.Unlock()
				for tag := range 256 {
					shard.takenTags[tag] = 0
					shard.returnedTags[tag] = 0
					shard.createdTags[tag] = 0
				}
			}()
		}
	}
}

func MessagePoolStats() map[int]map[int]float32 {
	sizeTagRatios := map[int]map[int]float32{}
	for _, pool := range orderedMessagePools() {
		tagRatios := map[int]float32{}
		for tag := range 256 {
			var taken uint64
			var returned uint64
			for _, shard := range pool.shards {
				func() {
					shard.mutex.Lock()
					defer shard.mutex.Unlock()
					taken += shard.takenTags[tag]
					returned += shard.returnedTags[tag]
				}()
			}
			if 0 < taken {
				ratio := float32(returned) / float32(taken)
				tagRatios[tag] = ratio
			}
		}
		sizeTagRatios[pool.size] = tagRatios
	}
	return sizeTagRatios
}

func MessagePoolReadAll(r io.Reader) ([]byte, error) {
	return MessagePoolReadAllWithTag(r, 0)
}

func MessagePoolReadAllWithTag(r io.Reader, tag uint8) ([]byte, error) {
	orderedMessagePools := orderedMessagePools()

	b, _ := MessagePoolGetDetailedWithTag(orderedMessagePools[0].size, tag)
	i := 0
	for j := 0; j < len(orderedMessagePools); j += 1 {
		for i < len(b) {
			n, err := r.Read(b[i:])
			if n > 0 {
				i += n
			}
			if err != nil {
				if err == io.EOF {
					return b[:i], nil
				}
				MessagePoolReturn(b)
				return nil, err
			}
			if n == 0 {
				return b[:i], nil
			}
		}

		if len(orderedMessagePools) <= j+1 {
			break
		}

		b2, _ := MessagePoolGetDetailedWithTag(orderedMessagePools[j+1].size, tag)
		copy(b2, b)
		MessagePoolReturn(b)
		b = b2
	}

	out := make([]byte, i, 2*i)
	copy(out, b)
	defer MessagePoolReturn(b)
	for {
		n, err := r.Read(b)
		if n > 0 {
			out = append(out, b[:n]...)
		}
		if err != nil {
			if err == io.EOF {
				return out, nil
			}
			// Preserve the historical contract that (non-EOF) errors yield a nil buffer
			// (callers do not expect to MessagePoolReturn on the error path).
			// We still consumed the bytes (preventing reader desync on streams).
			return nil, err
		}
		if n == 0 {
			return out, nil
		}
	}
}

func MessagePoolCopy(message []byte) []byte {
	b, _ := MessagePoolCopyDetailed(message)
	return b
}

func MessagePoolCopyDetailed(message []byte) ([]byte, bool) {
	var tag uint8
	if debugTags {
		tag = debugTag()
	}
	return MessagePoolCopyDetailedWithTag(message, tag)
}

func MessagePoolCopyDetailedWithTag(message []byte, tag uint8) ([]byte, bool) {
	poolMessage, pooled := MessagePoolGetDetailedWithTag(len(message), tag)
	copy(poolMessage, message)
	return poolMessage, pooled
}

func MessagePoolGet(n int) []byte {
	b, _ := MessagePoolGetDetailed(n)
	return b
}

func MessagePoolGetDetailed(n int) ([]byte, bool) {
	var tag uint8
	if debugTags {
		tag = debugTag()
	}
	return MessagePoolGetDetailedWithTag(n, tag)
}

func MessagePoolGetDetailedWithTag(n int, tag uint8) ([]byte, bool) {
	orderedMessagePools := orderedMessagePools()

	for _, pool := range orderedMessagePools {
		if n <= pool.size {
			poolMessage := pool.Get()
			c := poolMessage[pool.size+8] == 255
			poolMessage[pool.size+8] = tag
			id := binary.BigEndian.Uint64(poolMessage[pool.size:])

			shardIndex := int(poolMessage[pool.size+12])
			shard := pool.shard(shardIndex)

			func() {
				shard.mutex.Lock()
				defer shard.mutex.Unlock()

				if c {
					shard.createdTags[tag] += 1
				}
				shard.takenTags[tag] += 1

				count := binary.BigEndian.Uint16(poolMessage[pool.size+10:])

				if count != 0 {
					err := fmt.Errorf("message[%d] already taken", id)
					DefaultLogger().Errorf("[mp]%s", ErrorJson(err, debug.Stack()))
					panic(err)
				} else {
					binary.BigEndian.PutUint16(poolMessage[pool.size+10:], 1)
				}
			}()

			return poolMessage[:n], true
		}
	}
	// allocate a new message
	poolMessage := make([]byte, n+MessagePoolMetaByteCount)
	return poolMessage[:n], false
}

func MessagePoolReturn(message []byte) bool {
	orderedMessagePools := orderedMessagePools()

	c := cap(message)
	for _, pool := range orderedMessagePools {
		if c == pool.size+MessagePoolMetaByteCount {
			poolMessage := message[:c]
			id := binary.BigEndian.Uint64(poolMessage[pool.size:])

			tag := poolMessage[pool.size+8]
			shardIndex := int(poolMessage[pool.size+12])
			if shardIndex < 0 || shardIndex >= pool.shardCount {
				return false
			}
			shard := pool.shard(shardIndex)

			r := false
			func() {
				shard.mutex.Lock()
				defer shard.mutex.Unlock()

				count := binary.BigEndian.Uint16(poolMessage[pool.size+10:])
				if count == 0 {
					if debugTags {
						err := fmt.Errorf("[mp]return message[%d] not taken", id)
						DefaultLogger().Errorf("[mp]%s", ErrorJson(err, debug.Stack()))
					}
				} else if count == 1 {
					// reset metadata under the lock so a concurrent Share sees
					// count==0 and bails before the buffer reaches the freelist
					poolMessage[pool.size+8] = 0
					poolMessage[pool.size+9] = 0
					binary.BigEndian.PutUint16(poolMessage[pool.size+10:], 0)
					r = true
					shard.returnedTags[tag] += 1
				} else {
					binary.BigEndian.PutUint16(poolMessage[pool.size+10:], count-1)
				}
			}()

			if r {
				pool.Put(poolMessage)
				return true
			}
			return false
		}
	}
	// else drop the message, let it gc
	return false
}

func MessagePoolShareReadOnly(message []byte) []byte {
	orderedMessagePools := orderedMessagePools()

	c := cap(message)
	for _, pool := range orderedMessagePools {
		if c == pool.size+MessagePoolMetaByteCount {
			poolMessage := message[:c]
			id := binary.BigEndian.Uint64(poolMessage[pool.size:])

			shardIndex := int(poolMessage[pool.size+12])
			if shardIndex < 0 || shardIndex >= pool.shardCount {
				return message
			}
			shard := pool.shard(shardIndex)

			func() {
				shard.mutex.Lock()
				defer shard.mutex.Unlock()

				count := binary.BigEndian.Uint16(poolMessage[pool.size+10:])
				if count == 0 {
					DefaultLogger().Warningf("[mp]share message[%d] not taken", id)
				} else {
					binary.BigEndian.PutUint16(poolMessage[pool.size+10:], count+1)
					poolMessage[pool.size+9] |= MessagePoolFlagShared
				}
			}()

			return message
		}
	}
	// not a pool message
	return message
}

func MessagePoolCheck(message []byte) (pooled bool, shared bool) {
	orderedMessagePools := orderedMessagePools()

	c := cap(message)
	for _, pool := range orderedMessagePools {
		if c == pool.size+MessagePoolMetaByteCount {
			poolMessage := message[:c]

			shardIndex := int(poolMessage[pool.size+12])
			if shardIndex < 0 || shardIndex >= pool.shardCount {
				return
			}
			shard := pool.shard(shardIndex)

			func() {
				shard.mutex.Lock()
				defer shard.mutex.Unlock()

				count := binary.BigEndian.Uint16(poolMessage[pool.size+10:])
				if 0 < count {
					pooled = true
					shared = poolMessage[pool.size+9]&MessagePoolFlagShared != 0
				}
			}()

			return
		}
	}
	// not a pool message
	return
}

func ProtoMarshal(m proto.Message) ([]byte, error) {
	var tag uint8
	if debugTags {
		tag = debugTag()
	}
	return ProtoMarshalWithTag(m, tag)
}

func ProtoMarshalWithTag(m proto.Message, tag uint8) ([]byte, error) {
	if m == nil {
		return nil, nil
	}

	buf, _ := MessagePoolGetDetailedWithTag(proto.Size(m), tag)

	out, err := proto.MarshalOptions{}.MarshalAppend(buf[:0], m)
	if err != nil {
		MessagePoolReturn(buf)
		return nil, err
	}
	if cap(out) != cap(buf) {
		MessagePoolReturn(buf)
	}
	return out, nil
}

func ProtoUnmarshal(b []byte, m proto.Message) error {
	return proto.Unmarshal(b, m)
}

func EncodeBase64(enc *base64.Encoding, src []byte) string {
	buf := MessagePoolGet(enc.EncodedLen(len(src)))
	defer MessagePoolReturn(buf)
	enc.Encode(buf, src)
	return string(buf)
}

func DecodeBase64(enc *base64.Encoding, s string) ([]byte, error) {
	sbuf := MessagePoolGet(len(s))
	defer MessagePoolReturn(sbuf)
	copy(sbuf, s)
	buf := MessagePoolGet(enc.DecodedLen(len(s)))
	n, err := enc.Decode(buf, sbuf)
	if err != nil {
		MessagePoolReturn(buf)
		return nil, err
	}
	return buf[:n], nil
}

// EnhancedMetrics returns a JSON-friendly snapshot of the message pool
// performance counters (hits, misses, returns, active buffers, size
// distribution, GC pressure). It is exposed by the provider at
// /metrics/pool.
func EnhancedMetrics() map[string]any {
	totalPooled := uint64(0)
	for _, pool := range orderedMessagePools() {
		for _, shard := range pool.shards {
			func() {
				shard.mutex.Lock()
				defer shard.mutex.Unlock()
				totalPooled += uint64(shard.count)
			}()
		}
	}

	sizeDistribution := map[string]uint64{}
	for size, count := range globalPoolMetrics.SizeDistribution {
		sizeDistribution[fmt.Sprintf("%d", size)] = count.Load()
	}

	return map[string]any{
		"hits":              globalPoolMetrics.Hits.Load(),
		"misses":            globalPoolMetrics.Misses.Load(),
		"returns":           globalPoolMetrics.Returns.Load(),
		"active_buffers":    globalPoolMetrics.ActiveBuffers.Load(),
		"total_capacity":    globalPoolMetrics.TotalCapacity.Load(),
		"return_latency":    globalPoolMetrics.ReturnLatency.Load(),
		"return_count":      globalPoolMetrics.ReturnCount.Load(),
		"gc_pauses":         globalPoolMetrics.GCPauses.Load(),
		"pooled_buffers":    totalPooled,
		"size_distribution": sizeDistribution,
		"last_reset_time":   globalPoolMetrics.LastResetTime.Format(time.RFC3339),
	}
}

// sampleGCPauses is a lightweight sampler that tracks GC pause count for
// pool pressure reporting. It runs only while the process is alive.
func sampleGCPauses() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		globalPoolMetrics.GCPauses.Store(uint64(ms.NumGC))
	}
}

func init() {
	go sampleGCPauses()
}
