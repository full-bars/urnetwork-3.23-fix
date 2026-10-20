package connect

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
)

// The provider udp socket lifecycle shares a small fixed set of OS readiness
// workers per address family instead of parking one blocking socket reader, one
// send loop, and one idle timer per udp flow. A flow is permanently assigned to
// one shard, so its socket has exactly one drain producer and its datagrams
// keep their arrival order; the shard's bounded delivery queue is drained by a
// second worker, so a slow receive callback cannot stall the descriptor drain.
//
// The workers are created lazily by the owning buffer, so a tcp-only provider
// allocates no poll descriptor, worker stack, or read buffer. Backends are
// epoll on linux/android and kqueue on darwin/ios; every other platform returns
// no backend, and the buffer then keeps the established per-flow reader.
//
// This is the port of the shipped provider profile's shared socket lifecycle to
// this line. The fork has no receive dispatcher, no return-read lease/capacity
// layer, and no transfer memory budget on the udp nat, so the shipped pause/
// rearm coordinator and its explicit backend wake are not carried: the shard
// index is derived from the (immutable) flow path, delivery is one callback per
// datagram through the shard queue, and cancellation is observed by the
// readiness wait's bounded timeout.
const udpSocketPollEventCount = 64

// udpSocketPollTimeoutMillis bounds one readiness wait, so a shard observes
// parent cancellation without a backend-specific wake.
const udpSocketPollTimeoutMillis = 100

// udpSocketDatagramBatchCount is the largest number of datagrams one readiness
// event drains. It bounds the work a single always-ready descriptor can take
// from the other flows sharing the shard.
const udpSocketDatagramBatchCount = 64

// udpSocketPollEvent is one readiness notification: the descriptor that must be
// drained, and whether the backend also reported a terminal condition.
type udpSocketPollEvent struct {
	fd       int
	terminal bool
}

// udpSocketPollBackend is implemented with epoll on linux/android and kqueue on
// darwin/ios. The portable fallback returns a nil backend from
// newUdpSocketPollBackend, leaving UdpSequence's established reader intact.
type udpSocketPollBackend interface {
	add(fd int) error
	remove(fd int)
	wait(events []udpSocketPollEvent) (int, error)
	close() error
}

// udpSocketReadRegistration is one flow's socket inside a shard. The descriptor
// is pinned by rawConn for the complete drain, so a readiness event queued
// before unregister/close can never read a descriptor that has since been
// reused by another flow.
type udpSocketReadRegistration struct {
	fd       int
	sequence *UdpSequence
	socket   net.Conn
	rawConn  syscall.RawConn
}

// udpSocketDelivery is one drained datagram packet awaiting its receive
// callback. Ownership passes to the delivery worker; it returns the packet to
// the pool if the shard retires before delivery.
type udpSocketDelivery struct {
	sequence *UdpSequence
	packet   []byte
}

// udpSocketReadPollShard owns one readiness backend, the descriptor registry
// for the flows assigned to it, and the bounded delivery queue those flows
// share. mutex guards byFd only; the backend serializes itself.
type udpSocketReadPollShard struct {
	ctx        context.Context
	backend    udpSocketPollBackend
	readBuffer []byte
	mutex      sync.RWMutex
	byFd       map[int]*udpSocketReadRegistration
	deliveries chan udpSocketDelivery
}

type udpSocketReadPoller struct {
	shards    []udpSocketReadPollShard
	waitGroup sync.WaitGroup
}

// newUdpSocketReadPoller starts settings.SocketReadShardCount readiness shards,
// or returns nil when the profile does not enable the shared lifecycle or the
// platform has no backend.
func newUdpSocketReadPoller(
	ctx context.Context,
	settings *UdpBufferSettings,
) *udpSocketReadPoller {
	if settings == nil || settings.SocketReadShardCount <= 0 {
		return nil
	}
	shards := make([]udpSocketReadPollShard, 0, settings.SocketReadShardCount)
	for range settings.SocketReadShardCount {
		backend, err := newUdpSocketPollBackend()
		if err != nil {
			for i := range shards {
				_ = shards[i].backend.close()
			}
			return nil
		}
		shards = append(shards, udpSocketReadPollShard{
			ctx:        ctx,
			backend:    backend,
			readBuffer: make([]byte, max(1, settings.ReadBufferByteCount)),
			byFd:       map[int]*udpSocketReadRegistration{},
			deliveries: make(chan udpSocketDelivery, max(1, settings.SequenceBufferSize)),
		})
	}
	poller := &udpSocketReadPoller{shards: shards}
	for i := range poller.shards {
		shard := &poller.shards[i]
		poller.waitGroup.Add(2)
		go HandleError(func() {
			defer poller.waitGroup.Done()
			shard.run()
		})
		go HandleError(func() {
			defer poller.waitGroup.Done()
			shard.deliver()
		})
	}
	return poller
}

// Completion means every shard has stopped reading and delivering and has
// returned its queued packet owners.
func (self *udpSocketReadPoller) waitForLifecycle() {
	if self == nil {
		return
	}
	self.waitGroup.Wait()
}

// socketRawConn exposes the descriptor behind a dialed socket. The bandwidth
// wrapper embeds net.Conn and would otherwise hide SyscallConn.
func socketRawConn(socket net.Conn) (syscall.RawConn, bool) {
	if tracked, ok := socket.(*trackedConn); ok {
		socket = tracked.Conn
	}
	syscallConn, ok := socket.(syscall.Conn)
	if !ok {
		return nil, false
	}
	rawConn, err := syscallConn.SyscallConn()
	return rawConn, err == nil
}

// udpSocketReadShardIndex resolves a flow's permanent shard. The fork's local
// user nat already dispatches a flow to one buffer, so a flow's destination
// path (immutable for the flow's life) is the stable key here instead of the
// round-robin assignment a shared receive dispatcher would make.
func udpSocketReadShardIndex(sequence *UdpSequence, shardCount int) int {
	if shardCount <= 1 {
		return 0
	}
	hash := uint32(2166136261)
	hashBytes := func(value []byte) {
		for _, b := range value {
			hash ^= uint32(b)
			hash *= 16777619
		}
	}
	hashBytes(sequence.destinationIp)
	hashBytes([]byte{
		byte(sequence.destinationPort >> 8), byte(sequence.destinationPort),
		byte(sequence.sourcePort >> 8), byte(sequence.sourcePort),
	})
	return int(hash % uint32(shardCount))
}

// register adds one flow's socket to its shard. false means the flow must keep
// the portable per-flow reader.
func (self *udpSocketReadPoller) register(sequence *UdpSequence, socket net.Conn) bool {
	if self == nil || sequence == nil || socket == nil || len(self.shards) == 0 {
		return false
	}
	rawConn, ok := socketRawConn(socket)
	if !ok {
		return false
	}
	shardIndex := udpSocketReadShardIndex(sequence, len(self.shards))
	shard := &self.shards[shardIndex]
	fd := -1
	var registerErr error
	controlErr := rawConn.Control(func(rawFd uintptr) {
		fd = int(rawFd)
		shard.mutex.Lock()
		defer shard.mutex.Unlock()
		sequence.socketReadPollShard = shardIndex
		sequence.socketReadPollFd = fd
		// A descriptor number can be reused after a close; the newest
		// registration always owns the backend entry.
		if previous := shard.byFd[fd]; previous != nil {
			shard.backend.remove(fd)
		}
		shard.byFd[fd] = &udpSocketReadRegistration{
			fd:       fd,
			sequence: sequence,
			socket:   socket,
			rawConn:  rawConn,
		}
		registerErr = shard.backend.add(fd)
		if registerErr != nil {
			delete(shard.byFd, fd)
			sequence.socketReadPollFd = -1
		}
	})
	if controlErr != nil || registerErr != nil || fd < 0 {
		return false
	}
	return true
}

// unregister retires one flow's registration before its socket closes. The
// registration identity, not the reusable descriptor number, decides whether
// the entry still belongs to this flow.
func (self *udpSocketReadPoller) unregister(sequence *UdpSequence) {
	if self == nil || sequence == nil || len(self.shards) == 0 {
		return
	}
	shardIndex := sequence.socketReadPollShard
	if shardIndex < 0 || len(self.shards) <= shardIndex {
		return
	}
	fd := sequence.socketReadPollFd
	if fd < 0 {
		return
	}
	shard := &self.shards[shardIndex]
	shard.mutex.Lock()
	if registration, ok := shard.byFd[fd]; ok && registration.sequence == sequence {
		shard.backend.remove(fd)
		delete(shard.byFd, fd)
	}
	shard.mutex.Unlock()
	sequence.socketReadPollFd = -1
}

func (self *udpSocketReadPollShard) registration(fd int) (*udpSocketReadRegistration, bool) {
	self.mutex.RLock()
	defer self.mutex.RUnlock()
	registration, ok := self.byFd[fd]
	return registration, ok
}

func (self *udpSocketReadPollShard) run() {
	defer self.backend.close()
	events := make([]udpSocketPollEvent, udpSocketPollEventCount)
	for {
		select {
		case <-self.ctx.Done():
			return
		default:
		}
		eventCount, err := self.backend.wait(events)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			select {
			case <-self.ctx.Done():
				return
			default:
			}
			// a broken backend must not spin
			return
		}
		for _, event := range events[:eventCount] {
			registration, ok := self.registration(event.fd)
			if !ok {
				continue
			}
			readErr := self.drainReady(event.fd, registration)
			if event.terminal ||
				(readErr != nil && !errors.Is(readErr, syscall.EAGAIN) &&
					!errors.Is(readErr, syscall.EWOULDBLOCK)) {
				registration.sequence.Close()
			}
		}
	}
}

// deliver owns the receive callback for the shard, one datagram at a time and
// in queue order, so per-flow callback order is unchanged.
func (self *udpSocketReadPollShard) deliver() {
	for {
		select {
		case <-self.ctx.Done():
			self.returnQueued()
			return
		case delivery := <-self.deliveries:
			delivery.sequence.receivePacket(delivery.packet)
		}
	}
}

// Completion means no drained packet is left owned by the shard.
func (self *udpSocketReadPollShard) returnQueued() {
	for {
		select {
		case delivery := <-self.deliveries:
			MessagePoolReturn(delivery.packet)
		default:
			return
		}
	}
}

// drainReady pins the descriptor through the complete drain. A readiness event
// may already be queued when another goroutine unregisters and closes the
// socket; pinning prevents a descriptor reused by a new flow from being read
// into the old sequence.
func (self *udpSocketReadPollShard) drainReady(
	fd int,
	registration *udpSocketReadRegistration,
) error {
	if registration.rawConn == nil {
		return syscall.EBADF
	}
	var readErr error
	rawErr := registration.rawConn.Read(func(rawFd uintptr) bool {
		if int(rawFd) != fd {
			readErr = syscall.EBADF
			return true
		}
		readErr = self.readDatagrams(fd, registration.sequence)
		// The custom readiness backend, not the runtime poller, owns the next
		// wait. Even EAGAIN completes this RawConn operation.
		return true
	})
	if rawErr != nil {
		return rawErr
	}
	return readErr
}

// readDatagrams drains the datagrams that are already ready, up to a bounded
// batch, and hands each packet to the shard's delivery queue.
func (self *udpSocketReadPollShard) readDatagrams(fd int, sequence *UdpSequence) error {
	settings := sequence.udpBufferSettings
	for range udpSocketDatagramBatchCount {
		if sequence.ctx.Err() != nil {
			return syscall.EBADF
		}
		n, err := syscall.Read(SocketHandle(fd), self.readBuffer)
		if 0 < n {
			sequence.UpdateLastActivityTime()
			// the socket is read with a raw syscall, so the bandwidth wrapper
			// cannot credit the receive direction itself
			if sequence.bw != nil {
				sequence.bw.TotalRx.Add(uint64(n))
			}
			packets, packetsErr := sequence.DataPackets(self.readBuffer, n, settings.Mtu)
			if packetsErr != nil {
				return packetsErr
			}
			for _, packet := range packets {
				if !self.enqueue(sequence, packet) {
					MessagePoolReturn(packet)
					return syscall.EBADF
				}
			}
		}
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return err
		}
		if n == 0 {
			return nil
		}
	}
	return nil
}

// enqueue blocks until the shard's delivery worker has room or the buffer or
// the flow is canceled, exactly as the per-flow receive channel it replaces
// did. The kernel socket buffer is the next bounded absorption layer, so the
// shard never introduces a new datagram loss source.
func (self *udpSocketReadPollShard) enqueue(sequence *UdpSequence, packet []byte) bool {
	select {
	case <-self.ctx.Done():
		return false
	case <-sequence.ctx.Done():
		return false
	case self.deliveries <- udpSocketDelivery{sequence: sequence, packet: packet}:
		return true
	}
}
