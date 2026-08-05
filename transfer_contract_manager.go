package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	// "errors"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"

	// "slices"
	// "runtime/debug"
	mathrand "math/rand"

	"golang.org/x/exp/maps"

	// "google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// oobErrThrottle rate-limits `[contract]oob err`. During a control-API outage
// every sequence's CreateContract round-trip fails, so on a provider carrying
// many sequences this is one of the highest-volume lines in the log — and the
// one most likely to push out the lines needed to diagnose the outage. It is
// package-level, not per-ContractManager, because the flood is across every
// manager at once; a per-instance limiter would still emit once per proxy per
// interval, which at fleet scale is not a limit at all.
var oobErrThrottle = newLogThrottle(time.Minute)

// shouldLogOobErr reports whether a `[contract]oob err` line may be emitted
// now. The second return is the number of lines suppressed since the previous
// allowed one, for the "(N suppressed)" tail; see shouldLogAuthErr in
// transport.go for the full contract on that value.
func shouldLogOobErr() (bool, int64) { return oobErrThrottle.Allow(time.Now()) }

// Contract counters for aggregate metrics in heartbeat
var contractsAcquired uint64
var contractsDenied uint64
var contractUtilSum uint64

func ContractMetricsSnapshot() (acquired, denied, utilSum uint64) {
	acquired = atomic.SwapUint64(&contractsAcquired, 0)
	denied = atomic.SwapUint64(&contractsDenied, 0)
	utilSum = atomic.SwapUint64(&contractUtilSum, 0)
	return acquired, denied, utilSum
}

// manage contracts which are embedded into each transfer sequence

type ContractKey struct {
	Destination       TransferPath
	IntermediaryIds   MultiHopId
	CompanionContract bool
	ForceStream       bool
	// EncryptionRole separates the contract queues of the two per-peer
	// encryption send sequences to the same destination: the client-role
	// sequence (normal application data) and the server-role sequence
	// (EncryptedControl carrier + server replies). Without this, both would
	// share one queue, and one sequence's exit-flush (`FlushContractQueue`
	// on idle) would discard the other's pending contracts — starving the
	// handshake carrier. Zero value is client, so non-encrypted traffic and
	// legacy/pushed contracts key the same as before.
	EncryptionRole sequenceTlsRole
	// EncryptionCompanion separates the contract queues of two same-role send
	// sequences differing only by session identity companion — the two
	// server-role reply carriers that echo a companion vs non-companion
	// initiator both ride the same EncryptionControlUseCompanion contract, so
	// `CompanionContract` alone doesn't separate them. Without this they share a
	// queue and starve each other on exit-flush, as `EncryptionRole` guards for
	// the client/server split. Zero value false, so non-encrypted and
	// legacy/pushed contracts key as before.
	EncryptionCompanion bool
}

// Legacy returns the key reduced to its destination, discarding the
// intermediary, companion, force-stream, and encryption fields. The result
// keys the same dimension as contracts created before those fields existed.
func (self ContractKey) Legacy() ContractKey {
	return ContractKey{
		Destination: self.Destination,
	}
}

type ContractStatus struct {
	Key     ContractKey
	Error   *protocol.ContractError
	Premium bool
}

type ContractStatusFunction = func(ContractStatus *ContractStatus)

type contractStatusCallbackWorker struct {
	ctx                     context.Context
	cancel                  context.CancelFunc
	callback                ContractStatusFunction
	receiveContractStatuses chan *ContractStatus
}

func newContractStatusCallbackWorker(ctx context.Context, callback ContractStatusFunction, bufferSize int) *contractStatusCallbackWorker {
	callbackCtx, cancel := context.WithCancel(ctx)
	worker := &contractStatusCallbackWorker{
		ctx:                     callbackCtx,
		cancel:                  cancel,
		callback:                callback,
		receiveContractStatuses: make(chan *ContractStatus, bufferSize),
	}
	go HandleError(worker.run, cancel)
	return worker
}

// run is the worker goroutine loop: it delivers each queued contract status to
// the callback, wrapping each invocation in HandleError, and exits when the
// worker's context is canceled, leaving any still-queued statuses undelivered.
func (self *contractStatusCallbackWorker) run() {
	for {
		select {
		case <-self.ctx.Done():
			return
		case contractStatus := <-self.receiveContractStatuses:
			if self.ctx.Err() != nil {
				return
			}
			HandleError(func() {
				self.callback(contractStatus)
			})
		}
	}
}

// Dispatch queues a contract status for delivery on the worker goroutine,
// blocking while the channel is full. The callback runs on the worker's
// goroutine, never on the caller's; once the worker's context is canceled a
// status is never delivered (an in-flight send may still enqueue it, but the
// worker drops it).
func (self *contractStatusCallbackWorker) Dispatch(contractStatus *ContractStatus) {
	select {
	case <-self.ctx.Done():
	case self.receiveContractStatuses <- contractStatus:
	}
}

// Close cancels the worker's context, stopping its goroutine. Any statuses
// still queued are not delivered.
func (self *contractStatusCallbackWorker) Close() {
	self.cancel()
}

type ContractManagerStats struct {
	ContractOpenCount  int64
	ContractCloseCount int64
	// contract id -> byte count
	ContractOpenByteCounts map[Id]ByteCount
	// contract id -> contract key
	ContractOpenKeys              map[Id]ContractKey
	ContractCloseByteCount        ByteCount
	ReceiveContractCloseByteCount ByteCount
}

func NewContractManagerStats() *ContractManagerStats {
	return &ContractManagerStats{
		ContractOpenCount:             0,
		ContractCloseCount:            0,
		ContractOpenByteCounts:        map[Id]ByteCount{},
		ContractOpenKeys:              map[Id]ContractKey{},
		ContractCloseByteCount:        0,
		ReceiveContractCloseByteCount: 0,
	}
}

// ContractOpenByteCount returns the sum of the byte counts allotted to the
// contracts currently tracked as open. It performs no synchronization of its
// own, so call it on a snapshot taken from LocalStats rather than on a live
// manager's stats.
func (self *ContractManagerStats) ContractOpenByteCount() ByteCount {
	netContractOpenByteCount := ByteCount(0)
	for _, contractOpenByteCount := range self.ContractOpenByteCounts {
		netContractOpenByteCount += contractOpenByteCount
	}
	return netContractOpenByteCount
}

func SignStoredContract(settings *ContractManagerSettings, provideSecretKey []byte, storedContractBytes []byte) []byte {
	mac := hmac.New(sha256.New, provideSecretKey)
	if time.Now().Before(settings.NetworkEventTimeChangeHmac) {
		return mac.Sum(storedContractBytes)
	}
	mac.Write(storedContractBytes)
	return mac.Sum(nil)
}

func VerifyStoredContract(settings *ContractManagerSettings, provideSecretKey []byte, storedContractBytes []byte, storedContractHmac []byte) bool {
	legacyMac := hmac.New(sha256.New, provideSecretKey)
	if hmac.Equal(storedContractHmac, legacyMac.Sum(storedContractBytes)) {
		return true
	}
	standardMac := hmac.New(sha256.New, provideSecretKey)
	standardMac.Write(storedContractBytes)
	return hmac.Equal(storedContractHmac, standardMac.Sum(nil))
}

func DefaultContractManagerSettings() *ContractManagerSettings {
	return DefaultContractManagerSettingsWithBufferSize(defaultTransferBufferSize)
}

func DefaultContractManagerSettingsWithBufferSize(bufferSize int) *ContractManagerSettings {
	// NETWORK EVENT: at the enable contracts date, all clients will require contracts
	// up to that time, contracts are optional for the sender and match for the receiver
	networkEventTimeEnableContracts, err := time.Parse(time.RFC3339, "2024-05-01T00:00:00Z")
	if err != nil {
		panic(err)
	}
	networkEventTimeChangeHmac, err := time.Parse(time.RFC3339, "2026-09-01T00:00:00Z")
	if err != nil {
		panic(err)
	}
	return &ContractManagerSettings{
		SequenceBufferSize:                bufferSize,
		InitialContractTransferByteCount:  mib(2),
		StandardContractTransferByteCount: mib(128),
		ContractTransferByteSeqScale:      3,

		NetworkEventTimeEnableContracts: networkEventTimeEnableContracts,
		NetworkEventTimeChangeHmac:      networkEventTimeChangeHmac,

		ProvidePingTimeout: 0,

		OriginContractLinger: 300 * time.Second,

		ContractStatsEpoch: 60 * time.Second,

		ContractQueueExpireTimeout: 120 * time.Second,

		ProtocolVersion: DefaultProtocolVersion,
	}
}

func DefaultContractManagerSettingsNoNetworkEvents() *ContractManagerSettings {
	settings := DefaultContractManagerSettings()
	settings.NetworkEventTimeEnableContracts = time.Time{}
	settings.NetworkEventTimeChangeHmac = time.Time{}
	return settings
}

type ContractManagerSettings struct {
	SequenceBufferSize int

	// this should be enough to do a single ping
	InitialContractTransferByteCount  ByteCount
	StandardContractTransferByteCount ByteCount
	// scale up the contract size over this many contracts
	ContractTransferByteSeqScale uint64

	// enable contracts on the network
	// this can be removed after wide adoption
	NetworkEventTimeEnableContracts time.Time

	NetworkEventTimeChangeHmac time.Time

	// an active ping to the control fast-tracks any timeouts
	ProvidePingTimeout time.Duration

	// server-side companion policy: allow a return (companion) contract to be
	// created for up to this long after the origin contract in the opposite
	// direction was closed, so reply traffic can resume after the request side
	// goes idle. Reserved for future server-side enforcement.
	OriginContractLinger time.Duration

	// expire queued contracts that no sequence has taken within this window.
	// Bounds destinationContracts growth from orphans. <= 0 disables expiry.
	ContractQueueExpireTimeout time.Duration

	ProtocolVersion int

	ContractStatsEpoch time.Duration
}

// ContractsEnabled reports whether the network contracts-enablement event time
// has passed. While it has not, contracts are optional: senders may send
// without one and receivers accept traffic without one (see SendNoContract and
// ReceiveNoContract). A zero event time counts as enabled.
func (self *ContractManagerSettings) ContractsEnabled() bool {
	return self.NetworkEventTimeEnableContracts.Before(time.Now())
}

type ContractManager struct {
	ctx    context.Context
	client *Client

	settings *ContractManagerSettings

	mutex sync.Mutex

	// `provideSecretKeys` retains all keys until app restart (typically system restart)
	// this makes it faster for clients to reconnect with existing contracts
	// otherwise the client will have to time out the send sequence and flush its pending contracts
	provideSecretKeys map[protocol.ProvideMode][]byte
	provideModes      map[protocol.ProvideMode]bool
	// provide paused overrides the set provide modes
	providePaused  bool
	provideMonitor *Monitor

	destinationContracts map[ContractKey]*contractQueue

	receiveNoContractClientIds map[Id]bool
	sendNoContractClientIds    map[Id]bool

	contractStatusCallbacks *CallbackList[*contractStatusCallbackWorker]

	localStats *ContractManagerStats

	contractStatsLock      sync.Mutex
	contractStatsEntries   map[contractStatsKey]*contractStatsEntry
	contractStatsCallbacks *CallbackList[ContractStatsFunction]
	contractStatsStarted   bool

	controlSyncProvide    *ControlSync
	controlSyncProvideOob *ControlSyncOob
}

func NewContractManagerWithDefaults(ctx context.Context, client *Client) *ContractManager {
	return NewContractManager(ctx, client, DefaultContractManagerSettings())
}

func NewContractManager(
	ctx context.Context,
	client *Client,
	settings *ContractManagerSettings,
) *ContractManager {
	if settings.ContractStatsEpoch <= 0 {
		settings.ContractStatsEpoch = 10 * time.Second
	}
	// at a minimum
	// - messages to/from the platform (ControlId) do not need a contract
	//   this is because the platform is needed to create contracts
	// - messages to self do not need a contract
	receiveNoContractClientIds := map[Id]bool{
		ControlId:         true,
		client.ClientId(): true,
	}
	sendNoContractClientIds := map[Id]bool{
		ControlId:         true,
		client.ClientId(): true,
	}

	contractManager := &ContractManager{
		ctx:                        ctx,
		client:                     client,
		settings:                   settings,
		provideSecretKeys:          map[protocol.ProvideMode][]byte{},
		provideModes:               map[protocol.ProvideMode]bool{},
		providePaused:              false,
		provideMonitor:             NewMonitor(),
		destinationContracts:       map[ContractKey]*contractQueue{},
		receiveNoContractClientIds: receiveNoContractClientIds,
		sendNoContractClientIds:    sendNoContractClientIds,
		contractStatusCallbacks:    NewCallbackList[*contractStatusCallbackWorker](),
		localStats:                 NewContractManagerStats(),
		contractStatsEntries:       map[contractStatsKey]*contractStatsEntry{},
		contractStatsCallbacks:     NewCallbackList[ContractStatsFunction](),
		controlSyncProvide:         NewControlSync(ctx, client, "provide"),
		controlSyncProvideOob:      NewControlSyncOob(ctx, client, "provide-oob"),
	}

	if client.ClientId() != ControlId {
		go HandleError(contractManager.providePing, client.Cancel)
	}

	go HandleError(contractManager.expireQueuedContracts, client.Cancel)

	return contractManager
}

// providePing is the provide-ping loop started by NewContractManager for
// non-control clients. It periodically sends a ProvidePing control frame —
// keeping the control connection active so timeouts fast-track — and waits for
// each ack before the next ping. It sleeps a uniform interval with mean
// ProvidePingTimeout (0 disables pinging entirely), only pings while providing
// is enabled and not paused (waking on provideMonitor notifications), and
// exits when the manager context is canceled.
func (self *ContractManager) providePing() {
	if self.settings.ProvidePingTimeout == 0 {
		return
	}

	// Wait for the client to finish wiring before our first send. This
	// goroutine is started from `NewContractManager`, which runs inside
	// `NewClientWithTag` before `initBuffers` constructs `sendBuffer`.
	// Without this gate the ping path can race the buffer wiring.
	select {
	case <-self.client.ReadyNotify():
	case <-self.ctx.Done():
		return
	}

	// used for logging states only
	logWait := false

	waitForProvide := func() bool {
		for {
			notify := self.provideMonitor.NotifyChannel()
			var provide bool
			func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()

				if self.providePaused {
					provide = false
				} else {
					provide = self.provideModes[protocol.ProvideMode_Public] || self.provideModes[protocol.ProvideMode_PublicStream]
				}
			}()
			if provide {
				if logWait {
					logWait = false
					self.client.log.Infof("[contract]provide ping continue\n")
				}
				return true
			}
			if !logWait {
				logWait = true
				self.client.log.Infof("[contract]provide ping wait\n")
			}
			select {
			case <-self.ctx.Done():
				return false
			case <-notify:
			}
		}
	}

	lastPingTime := time.Time{}
	for {
		if !waitForProvide() {
			return
		}

		// uniform timeout with mean `ProvidePingTimeout`
		timeout := time.Duration(mathrand.Int63n(int64(2*self.settings.ProvidePingTimeout))) - time.Now().Sub(lastPingTime)
		if 0 < timeout {
			select {
			case <-self.ctx.Done():
				return
			case <-WakeupAfter(timeout, self.settings.ProvidePingTimeout):
			}
		} else {
			select {
			case <-self.ctx.Done():
				return
			default:
			}
		}

		ack := make(chan error)
		providePing := &protocol.ProvidePing{}
		frame, err := ToFrame(providePing, self.settings.ProtocolVersion)
		if err != nil {
			self.client.log.Infof("[contract]could not create provide ping frame = %s", err)
			return
		}
		self.client.SendControl(frame, func(err error) {
			select {
			case ack <- err:
			case <-self.ctx.Done():
			}
		})
		// wait for the ack before sending another ping
		select {
		case err := <-ack:
			if err != nil {
				self.client.log.Infof("[contract]provide ping err = %s\n", err)
			}
		case <-self.ctx.Done():
			return
		}
		lastPingTime = time.Now()
	}
}

// expireQueuedContracts periodically closes queued contracts that no sequence
// took within ContractQueueExpireTimeout and removes the emptied queues.
// On shutdown it immediately closes all still-pending contracts so their
// escrow is released rather than timing out server-side.
func (self *ContractManager) expireQueuedContracts() {
	timeout := self.settings.ContractQueueExpireTimeout

	finalFlush := func() {
		pending := []*protocol.Contract{}
		func() {
			self.mutex.Lock()
			defer self.mutex.Unlock()

			for contractKey, contractQueue := range self.destinationContracts {
				pending = append(pending, contractQueue.Flush(false)...)
				if contractQueue.IsDone() {
					delete(self.destinationContracts, contractKey)
				}
			}
		}()
		if 0 < len(pending) {
			self.client.log.V(1).Infof("[contract]closing %d pending contracts on close\n", len(pending))
			self.closeContracts(pending)
		}
	}

	for {
		// when expiry is disabled the nil tick channel blocks forever and
		// the loop only exits on shutdown
		var tick <-chan time.Time
		if 0 < timeout {
			tick = time.After(timeout / 2)
		}

		select {
		case <-self.ctx.Done():
			finalFlush()
			return
		case <-self.client.Done():
			finalFlush()
			return
		case <-tick:
		}

		minEnqueueTime := time.Now().Add(-timeout)
		expired := []*protocol.Contract{}
		func() {
			self.mutex.Lock()
			defer self.mutex.Unlock()

			for contractKey, contractQueue := range self.destinationContracts {
				expired = append(expired, contractQueue.Expire(minEnqueueTime)...)
				if contractQueue.IsDone() {
					delete(self.destinationContracts, contractKey)
				}
			}
		}()
		if 0 < len(expired) {
			self.client.log.V(1).Infof("[contract]expired %d queued contracts\n", len(expired))
			self.closeContracts(expired)
		}
	}
}

// StandardContractTransferByteCount returns the settings value for a fully
// scaled-up contract's byte allowance, the size contractByteCount converges
// to.
func (self *ContractManager) StandardContractTransferByteCount() ByteCount {
	return self.settings.StandardContractTransferByteCount
}

// AddContractStatusCallback registers a callback for contract status events
// and returns the unsubscribe function. The caller must retain the returned
// function and invoke it on teardown — discarding it leaks the registration:
// the callback keeps receiving events, and its worker goroutine keeps
// running, until the manager's context is canceled (bounded by the manager's
// lifetime, not a process-lifetime leak). Each event is one ContractStatus
// for one contract result: acquired (Error nil, Premium taken from the stored
// contract's priority), rejected, invalid, or denied (Error set). The
// callback runs on its own goroutine, never on the caller's, and receives
// statuses in order, buffered up to the sequence buffer size.
func (self *ContractManager) AddContractStatusCallback(contractStatusCallback ContractStatusFunction) func() {
	worker := newContractStatusCallbackWorker(self.ctx, contractStatusCallback, self.settings.SequenceBufferSize)
	callbackId := self.contractStatusCallbacks.Add(worker)
	return func() {
		self.contractStatusCallbacks.Remove(callbackId)
		worker.Close()
	}
}

// ContractStatusFunction
func (self *ContractManager) contractStatus(contractStatus *ContractStatus) {
	for _, contractStatusCallback := range self.contractStatusCallbacks.Get() {
		contractStatusCallback.Dispatch(contractStatus)
	}
}

/*
// ReceiveFunction
func (self *ContractManager) Receive(source TransferPath, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
	if source.IsControlSource() {
		for _, frame := range frames {
			self.handleControlFrame(nil, frame)
		}
	}
}
*/

// HandleControlFrame handles a control frame from the platform's contract
// result path. It processes TransferCreateContractResult frames: each parsed
// contract is queued via addContract and reported to the contract-status
// callbacks as acquired (with Premium from the stored priority), rejected, or
// invalid; each platform denial is reported as a denied status. Any other
// message type is ignored. The return value is always nil; per-contract
// errors are reported through the status callbacks instead.
func (self *ContractManager) HandleControlFrame(contractKey ContractKey, frame *protocol.Frame) error {
	switch frame.MessageType {
	case protocol.MessageType_TransferCreateContractResult:
		contracts, contractErrors := self.parseControlFrame(frame)
		for _, contract := range contracts {
			c := func() error {
				err := self.addContract(contractKey, contract)
				var contractStatus *ContractStatus
				if err != nil {
					// contract rejected
					contractError := protocol.ContractError_Trust
					contractStatus = &ContractStatus{
						Key:   contractKey,
						Error: &contractError,
					}
					self.contractStatus(contractStatus)
					return err
				} else {
					storedContract := &protocol.StoredContract{}
					err = ProtoUnmarshal(contract.StoredContractBytes, storedContract)
					if err != nil {
						contractError := protocol.ContractError_Invalid
						contractStatus = &ContractStatus{
							Key:   contractKey,
							Error: &contractError,
						}
						self.contractStatus(contractStatus)
						return err
					} else {
						premium := false
						if storedContract.Priority != nil {
							premium = 0 < *storedContract.Priority
						}
						contractStatus = &ContractStatus{
							Key:     contractKey,
							Premium: premium,
						}

						self.contractStatus(contractStatus)
						self.client.log.Infof("🤝 [contract] acquired size=%s destination=%s\n",
							ByteCountHumanReadable(ByteCount(storedContract.GetTransferByteCount())),
							contractKey.Destination.DestinationId)
						atomic.AddUint64(&contractsAcquired, 1)
						return nil
					}
				}
			}
			if self.client.log.V(2).Enabled() {
				TraceWithReturn(
					"[contract]add",
					c,
				)
			} else {
				c()
			}
		}
		for _, contractError := range contractErrors {
			self.client.log.Infof("⛔ [contract] denied = %s destination=%s\n", contractError, contractKey.Destination.DestinationId)
			atomic.AddUint64(&contractsDenied, 1)
			c := func() {
				contractStatus := &ContractStatus{
					Key:   contractKey,
					Error: &contractError,
				}

				self.contractStatus(contractStatus)
			}
			if self.client.log.V(2).Enabled() {
				Trace(
					fmt.Sprintf("[contract]error = %s", contractError),
					c,
				)
			} else {
				c()
			}
		}
	}
	return nil
}

// frames are verified before calling to be from source ControlId
func (self *ContractManager) parseControlFrame(frame *protocol.Frame) (
	contracts []*protocol.Contract,
	contractErrors []protocol.ContractError,
) {
	addResult := func(v *protocol.CreateContractResult) {
		if contractError := v.Error; contractError != nil {
			contractErrors = append(contractErrors, *contractError)
		} else if contract := v.Contract; contract != nil {
			storedContract := &protocol.StoredContract{}
			err := ProtoUnmarshal(contract.StoredContractBytes, storedContract)
			if err != nil {
				return
			}

			contracts = append(contracts, contract)
		}
	}

	switch frame.MessageType {
	case protocol.MessageType_TransferCreateContractResult:
		b := make([]byte, len(frame.MessageBytes))
		copy(b, frame.MessageBytes)
		r := &protocol.CreateContractResult{}
		err := ProtoUnmarshal(b, r)
		if err == nil {
			addResult(r)
		}
	}
	return
}

// GetProvideSecretKeys returns a copy of the mode-to-secret-key map. Adding or
// removing entries in the result does not affect the manager; the key byte
// slices themselves are shared with the manager, so they must not be mutated.
func (self *ContractManager) GetProvideSecretKeys() map[protocol.ProvideMode][]byte {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return maps.Clone(self.provideSecretKeys)
}

// LoadProvideSecretKeys merges persisted provide secret keys into the retained
// set, overwriting per mode and leaving modes absent from the argument intact.
// Keys are retained until process restart so reconnecting clients can reuse
// their contracts without timing out their send sequence.
func (self *ContractManager) LoadProvideSecretKeys(provideSecretKeys map[protocol.ProvideMode][]byte) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	for provideMode, provideSecretKey := range provideSecretKeys {
		self.provideSecretKeys[provideMode] = provideSecretKey
	}
}

// InitProvideSecretKeys ensures every ProvideMode has a secret key, generating
// a fresh 32-byte key for any mode that has none. Existing keys are retained.
// It panics if the OS random source fails.
func (self *ContractManager) InitProvideSecretKeys() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	for i, _ := range protocol.ProvideMode_name {
		provideMode := protocol.ProvideMode(i)
		provideSecretKey, ok := self.provideSecretKeys[provideMode]
		if !ok {
			// generate a new key
			provideSecretKey = make([]byte, 32)
			_, err := rand.Read(provideSecretKey)
			if err != nil {
				panic(err)
			}
			self.provideSecretKeys[provideMode] = provideSecretKey
		}
	}
}

// SetProvidePaused pauses or resumes providing. While paused, the public
// provide modes stop being advertised; Stream (return traffic) and Network
// keep working (see provideFrame and Verify). It returns true only when the
// state actually changed; on a change it wakes waiters on the provide monitor
// and, when the provide frame can be built, sends it over the in-band control
// sync. The manager mutex makes the change visible to concurrent readers such
// as providePing and Verify.
func (self *ContractManager) SetProvidePaused(providePaused bool) bool {
	changed := false
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		if self.providePaused != providePaused {
			self.providePaused = providePaused
			self.provideMonitor.NotifyAll()
			changed = true
		}
	}()
	if changed {
		if provideFrame, err := self.provideFrame(); err == nil && provideFrame != nil {
			self.controlSyncProvide.Send(
				provideFrame,
				nil,
				nil,
			)
		}
		return true
	}
	return false
}

// IsProvidePaused reports whether providing is currently paused. It may be
// called concurrently with SetProvidePaused.
func (self *ContractManager) IsProvidePaused() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return self.providePaused
}

// provideFrame builds the Provide frame advertising the current provide keys.
// When paused, only the Stream and Network mode keys are advertised (return
// traffic and network peers keep working while public and friends-and-family
// stop); otherwise every enabled mode that has a key is advertised, and
// enabled modes missing a key are omitted with a log. On frame-encoding
// failure it returns nil and the error.
func (self *ContractManager) provideFrame() (*protocol.Frame, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	var provide *protocol.Provide
	if self.providePaused {
		// pause stops providing to public/ff only.
		// keep ProvideMode_Stream to allow return traffic and
		// ProvideMode_Network so network peers never fall back to stream, if set
		provideKeys := []*protocol.ProvideKey{}
		for provideMode, allow := range self.provideModes {
			if allow && (provideMode == protocol.ProvideMode_Stream || provideMode == protocol.ProvideMode_Network) {
				provideSecretKey, ok := self.provideSecretKeys[provideMode]
				if ok {
					provideKeys = append(provideKeys, &protocol.ProvideKey{
						Mode:             provideMode,
						ProvideSecretKey: provideSecretKey,
					})
				} else {
					self.client.log.Infof("[contract]missing provide key for %d. Will omit.\n", provideMode)
				}
			}
		}

		provide = &protocol.Provide{
			Keys: provideKeys,
		}
	} else {
		provideKeys := []*protocol.ProvideKey{}
		for provideMode, allow := range self.provideModes {
			if allow {
				provideSecretKey, ok := self.provideSecretKeys[provideMode]
				if ok {
					provideKeys = append(provideKeys, &protocol.ProvideKey{
						Mode:             provideMode,
						ProvideSecretKey: provideSecretKey,
					})
				} else {
					self.client.log.Infof("[contract]missing provide key for %d. Will omit.\n", provideMode)
				}
			}
		}

		provide = &protocol.Provide{
			Keys: provideKeys,
		}
	}
	provideFrame, err := ToFrame(provide, self.settings.ProtocolVersion)
	if err != nil {
		self.client.log.Infof("[contract]could not create provide frame = %s", err)
		return nil, err
	}
	return provideFrame, nil
}

// SetProvideModesWithReturnTraffic sets the provide modes with
// ProvideMode_Stream forced on, so return traffic is allowed (clients must
// enable Stream to receive replies), and registers the change over the
// in-band control with a no-op ack callback.
func (self *ContractManager) SetProvideModesWithReturnTraffic(provideModes map[protocol.ProvideMode]bool) {
	self.SetProvideModesWithReturnTrafficWithAckCallback(provideModes, func(err error) {})
}

// clients must enable `ProvideMode_Stream` to allow return traffic
func (self *ContractManager) SetProvideModesWithReturnTrafficWithAckCallback(provideModes map[protocol.ProvideMode]bool, ackCallback func(err error)) {
	updatedProvideModes := map[protocol.ProvideMode]bool{}
	maps.Copy(updatedProvideModes, provideModes)
	updatedProvideModes[protocol.ProvideMode_Stream] = true
	self.SetProvideModesWithAckCallback(updatedProvideModes, ackCallback)
}

// SetProvideModes replaces the active provide modes with the given set
// (missing secret keys are generated) and registers the change over the
// in-band control with a no-op ack callback. Modes not in the map are no
// longer provided.
func (self *ContractManager) SetProvideModes(provideModes map[protocol.ProvideMode]bool) {
	self.SetProvideModesWithAckCallback(provideModes, func(err error) {})
}

// applyProvideModes generates any missing provide secret keys and updates the
// active provide modes. The provide frame must be (re)sent afterward to register
// the change with the platform.
func (self *ContractManager) applyProvideModes(provideModes map[protocol.ProvideMode]bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// keep all keys (see note on `provideSecretKeys`)
	for provideMode, allow := range provideModes {
		if allow {
			provideSecretKey, ok := self.provideSecretKeys[provideMode]
			if !ok {
				// generate a new key
				provideSecretKey = make([]byte, 32)
				_, err := rand.Read(provideSecretKey)
				if err != nil {
					panic(err)
				}
				self.provideSecretKeys[provideMode] = provideSecretKey
			}
		}
	}

	self.provideModes = maps.Clone(provideModes)
	self.provideMonitor.NotifyAll()
}

// SetProvideModesWithAckCallback replaces the active provide modes and sends
// the updated provide frame over the in-band control sync, invoking
// ackCallback with the send result. The callback fires once per call, not per
// contract or frame: synchronously with the error when the provide frame
// cannot be built, otherwise asynchronously when the control send completes.
// An in-band ack means the frame was delivered, not that the platform has
// committed the secret — use the out-of-band variant when registration must
// be confirmed.
func (self *ContractManager) SetProvideModesWithAckCallback(provideModes map[protocol.ProvideMode]bool, ackCallback func(err error)) {
	self.applyProvideModes(provideModes)
	if provideFrame, err := self.provideFrame(); err != nil {
		ackCallback(err)
	} else if provideFrame != nil {
		self.controlSyncProvide.Send(
			provideFrame,
			nil,
			ackCallback,
		)
	} else {
		ackCallback(nil)
	}
}

// SetProvideModesWithReturnTrafficWithOobAckCallback is like
// SetProvideModesWithReturnTrafficWithAckCallback, but registers the provide via
// the out-of-band control, so the ack means the platform has committed the
// provide secret (the in-band control ack only means the message was delivered).
// Use this when a caller must wait for the secret to be registered before using
// the client — e.g. the return path of a multi-client client, whose companion
// (Stream) contracts are verified against this secret.
func (self *ContractManager) SetProvideModesWithReturnTrafficWithOobAckCallback(provideModes map[protocol.ProvideMode]bool, ackCallback func(err error)) {
	updatedProvideModes := map[protocol.ProvideMode]bool{}
	maps.Copy(updatedProvideModes, provideModes)
	updatedProvideModes[protocol.ProvideMode_Stream] = true
	self.SetProvideModesWithOobAckCallback(updatedProvideModes, ackCallback)
}

// SetProvideModesWithOobAckCallback is like SetProvideModesWithAckCallback but
// registers the provide over the out-of-band control sync, where the ack means
// the platform has committed the provide secret rather than merely delivered
// the message.
func (self *ContractManager) SetProvideModesWithOobAckCallback(provideModes map[protocol.ProvideMode]bool, ackCallback func(err error)) {
	self.applyProvideModes(provideModes)
	if provideFrame, err := self.provideFrame(); err != nil {
		ackCallback(err)
	} else if provideFrame != nil {
		self.controlSyncProvideOob.Send(
			provideFrame,
			ackCallback,
		)
	} else {
		ackCallback(nil)
	}
}

// GetProvideModes returns a copy of the active provide modes, so the caller
// may mutate the result without affecting the manager.
func (self *ContractManager) GetProvideModes() map[protocol.ProvideMode]bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return maps.Clone(self.provideModes)
}

// Verify reports whether a contract presented by a remote client is
// trustworthy: the mode must be enabled (unless paused, which keeps Stream and
// Network valid), a secret key must exist for it, and the stored-contract HMAC
// must match under either the legacy appended-HMAC format or the standard pure
// HMAC format. This is the provider-side check applied before accepting a
// contract.
func (self *ContractManager) Verify(storedContractHmac []byte, storedContractBytes []byte, provideMode protocol.ProvideMode) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// when paused, only allow ProvideMode_Stream for return traffic and
	// ProvideMode_Network for network peers (pause stops public/ff only)
	if self.providePaused && provideMode != protocol.ProvideMode_Stream && provideMode != protocol.ProvideMode_Network {
		return false
	}

	if !self.provideModes[provideMode] {
		return false
	}

	provideSecretKey, ok := self.provideSecretKeys[provideMode]
	if !ok {
		// provide mode is not enabled
		return false
	}

	// Try legacy format first (pre-July 1, 2026): mac.Sum(data) appends HMAC to data
	mac := hmac.New(sha256.New, provideSecretKey)
	expectedHmac := mac.Sum(storedContractBytes)
	if hmac.Equal(storedContractHmac, expectedHmac) {
		return true
	}

	// Try standard format (post-July 1, 2026): mac.Write(data); mac.Sum(nil) returns pure HMAC
	mac.Reset()
	mac.Write(storedContractBytes)
	expectedHmac = mac.Sum(nil)
	return hmac.Equal(storedContractHmac, expectedHmac)
}

// GetProvideSecretKey returns the secret key for a provide mode. The second
// result is false when the mode is not enabled or no key has been generated
// for it.
func (self *ContractManager) GetProvideSecretKey(provideMode protocol.ProvideMode) ([]byte, bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if !self.provideModes[provideMode] {
		return nil, false
	}

	provideSecretKey, ok := self.provideSecretKeys[provideMode]
	return provideSecretKey, ok
}

// RequireProvideSecretKey returns the secret key for a provide mode, panicking
// when the mode is not enabled or has no key. It never returns an error.
func (self *ContractManager) RequireProvideSecretKey(provideMode protocol.ProvideMode) []byte {
	secretKey, ok := self.GetProvideSecretKey(provideMode)
	if !ok {
		panic(fmt.Errorf("Missing provide secret for %s", provideMode))
	}
	return secretKey
}

// AddNoContractPeer registers a peer client id as a no-contract peer: traffic
// to and from it is allowed without a contract. No-contract is mutual — both
// sides must register each other.
func (self *ContractManager) AddNoContractPeer(clientId Id) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.sendNoContractClientIds[clientId] = true
	self.receiveNoContractClientIds[clientId] = true
}

// SendNoContract reports whether a send to destinationId may proceed without
// a contract: true for registered no-contract peers (see AddNoContractPeer)
// and, while contracts are not yet enabled network-wide, true for everyone;
// false otherwise.
func (self *ContractManager) SendNoContract(destinationId Id) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if allow, ok := self.sendNoContractClientIds[destinationId]; ok {
		return allow
	}

	if !self.settings.ContractsEnabled() {
		return true
	}

	return false
}

// ReceiveNoContract reports whether traffic from sourceId may be accepted
// without a contract: true for registered no-contract peers (see
// AddNoContractPeer) and, while contracts are not yet enabled network-wide,
// true for everyone; false otherwise.
func (self *ContractManager) ReceiveNoContract(sourceId Id) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if allow, ok := self.receiveNoContractClientIds[sourceId]; ok {
		return allow
	}

	if !self.settings.ContractsEnabled() {
		return true
	}

	return false
}

// TakeContract takes a queued contract for the key, consuming it from the
// queue. A negative timeout waits indefinitely, a zero timeout polls once
// without waiting, and a positive timeout bounds the wait; in all cases a nil
// result means no contract arrived before the deadline or the context was
// canceled (expired contracts are closed on the way out). On success the
// contract is recorded in the local open-contract stats.
func (self *ContractManager) TakeContract(
	ctx context.Context,
	contractKey ContractKey,
	timeout time.Duration,
) *protocol.Contract {
	contractQueue := self.openContractQueue(contractKey)
	defer self.closeContractQueue(contractKey)

	enterTime := time.Now()
	for {
		notify := contractQueue.updateMonitor.NotifyChannel()
		var minEnqueueTime time.Time
		if 0 < self.settings.ContractQueueExpireTimeout {
			minEnqueueTime = time.Now().Add(-self.settings.ContractQueueExpireTimeout)
		}
		contract, expired := contractQueue.Poll(minEnqueueTime)
		if 0 < len(expired) {
			self.closeContracts(expired)
		}

		if contract != nil {
			storedContract := &protocol.StoredContract{}
			if err := ProtoUnmarshal(contract.StoredContractBytes, storedContract); err == nil {
				if contractId, err := IdFromBytes(storedContract.ContractId); err == nil {
					func() {
						self.mutex.Lock()
						defer self.mutex.Unlock()

						self.localStats.ContractOpenCount += 1
						self.localStats.ContractOpenByteCounts[contractId] = ByteCount(storedContract.TransferByteCount)
						self.localStats.ContractOpenKeys[contractId] = contractKey
					}()
				}
			}

			return contract
		}

		if timeout < 0 {
			select {
			case <-self.ctx.Done():
				return nil
			case <-ctx.Done():
				return nil
			case <-notify:
			}
		} else if timeout == 0 {
			return nil
		} else {
			remainingTimeout := enterTime.Add(timeout).Sub(time.Now())
			if remainingTimeout <= 0 {
				return nil
			}
			select {
			case <-self.ctx.Done():
				return nil
			case <-ctx.Done():
				return nil
			case <-notify:
			case <-time.After(remainingTimeout):
				return nil
			}
		}
	}
}

// addContract validates a contract received from the platform and queues it
// for the contract key. It fails when the stored contract bytes do not parse
// or when the stored contract's source id is not this client's id. On success
// the contract is added to the key's queue: an entry for the same contract id
// replaces the queued one and, when the queue tracks used ids, an already-used
// id is dropped instead.
func (self *ContractManager) addContract(contractKey ContractKey, contract *protocol.Contract) error {
	storedContract := &protocol.StoredContract{}
	err := ProtoUnmarshal(contract.StoredContractBytes, storedContract)
	if err != nil {
		return err
	}

	if Id(storedContract.SourceId) != self.client.ClientId() {
		return fmt.Errorf("Contract source must be this client: %s<>%s", Id(storedContract.SourceId), self.client.ClientId())
	}

	self.client.log.V(1).Infof("[contract]add %s %s\n", self.client.ClientId(), contractKey.Destination)

	func() {
		contractQueue := self.openContractQueue(contractKey)
		defer self.closeContractQueue(contractKey)
		contractQueue.Add(contract, storedContract)
	}()

	return nil
}

// CreateContract requests a new contract for the key from the platform over
// the out-of-band control. The requested byte count is contractByteCount at
// the given sequence index, bounded below by minByteCount. The request is
// asynchronous: the result arrives later through HandleControlFrame, which
// either queues the contract for TakeContract or reports a denial to the
// contract-status callbacks; this function itself returns nothing and only
// logs a frame-build failure. Each round-trip feeds the backend degradation
// state (noteBackendSuccess on success, noteBackendFailure otherwise); while
// the backend is degraded the send sequence skips issuing new requests (see
// isBackendDegraded).
func (self *ContractManager) CreateContract(contractKey ContractKey, contractSeqIndex uint64, minByteCount ByteCount) {
	// look at destinationContracts and last contract to get previous contract id
	self.openContractQueue(contractKey)
	defer self.closeContractQueue(contractKey)

	streamVersion := uint32(DefaultStreamVersion)

	createContract := &protocol.CreateContract{
		DestinationId:     contractKey.Destination.DestinationId.Bytes(),
		IntermediaryIds:   contractKey.IntermediaryIds.Bytes(),
		TransferByteCount: uint64(self.contractByteCount(contractSeqIndex, minByteCount)),
		Companion:         contractKey.CompanionContract,
		ForceStream:       &contractKey.ForceStream,
		StreamVersion:     &streamVersion,
	}
	frame, err := ToFrame(createContract, self.settings.ProtocolVersion)
	if err != nil {
		self.client.log.Infof("[contract]could not create contract frame = %s", err)
		return
	}

	self.client.log.V(1).Infof("[contract]create %s %s\n", self.client.ClientId(), contractKey.Destination)

	self.client.ClientOob().SendControl(
		[]*protocol.Frame{frame},
		func(resultFrames []*protocol.Frame, err error) {
			if err == nil {
				noteBackendSuccess()
				// [contract] acquired is logged in HandleControlFrame on the
				// actual contract-success branch, not here: err==nil only means
				// the control request round-tripped; the result may still carry a
				// ContractError (denial). Logging here would double-log denials.
				for _, resultFrame := range resultFrames {
					self.HandleControlFrame(contractKey, resultFrame)
				}
			} else {
				select {
				case <-self.client.Done():
					// no need to log warnings when the client closes
				default:
					noteBackendFailure()
					if ok, suppressed := shouldLogOobErr(); ok {
						if suppressed > 0 {
							self.client.log.Infof("[contract]oob err = %s (%d suppressed)\n", err, suppressed)
						} else {
							self.client.log.Infof("[contract]oob err = %s\n", err)
						}
					}
				}
			}
		},
	)
}

// contractByteCount returns the byte count to request for the contract at the
// given sequence index: a lerp from the initial to the standard byte count
// over the first ContractTransferByteSeqScale contracts, then the standard
// count, never below minByteCount. A zero scale jumps straight to the
// standard count.
func (self *ContractManager) contractByteCount(contractSeqIndex uint64, minByteCount ByteCount) ByteCount {
	scale := self.settings.ContractTransferByteSeqScale
	if scale == 0 {
		return max(self.settings.StandardContractTransferByteCount, minByteCount)
	}
	targetByteCount := func() ByteCount {
		if scale <= contractSeqIndex {
			return self.settings.StandardContractTransferByteCount
		} else {
			// lerp between initial and standard
			return self.settings.InitialContractTransferByteCount + ByteCount(
				(contractSeqIndex*uint64(self.settings.StandardContractTransferByteCount-self.settings.InitialContractTransferByteCount))/scale,
			)
		}
	}()
	return max(targetByteCount, minByteCount)
}

// CheckpointContract reports acked and unacked byte counts for an open
// contract without closing it: the wire contract remains open so the sender
// may keep using it (used by the receive sequence when a contract may be
// reused). The local open-contract stats are left intact; only the per-contract
// stats entry is released.
func (self *ContractManager) CheckpointContract(
	contractId Id,
	ackedByteCount ByteCount,
	unackedByteCount ByteCount,
) {
	self.CloseContractWithCheckpoint(contractId, ackedByteCount, unackedByteCount, true)
}

// CloseContract closes an open contract, finalizing the local open-contract
// stats (close count and acked byte total) and sending a CloseContract frame
// with the acked and unacked byte counts. When the close send succeeds and
// the contract was opened through the manager, its id is released from the
// queue's used set, so a fresh contract with the same id can be queued again.
func (self *ContractManager) CloseContract(
	contractId Id,
	ackedByteCount ByteCount,
	unackedByteCount ByteCount,
) {
	self.CloseContractWithCheckpoint(contractId, ackedByteCount, unackedByteCount, false)
}

// CloseContractWithCheckpoint is the shared implementation behind
// CloseContract and CheckpointContract. It sends a CloseContract frame (with
// the Checkpoint flag; a frame-build failure is only logged) and always
// releases the per-contract stats entry. With checkpoint false it also
// finalizes the open-contract stats — close count, acked byte total, and
// utilization — and, once the close send succeeds, releases the contract id
// from the queue's used set; with checkpoint true the open stats stay
// untouched because the wire contract may be reused by a new sequence.
// Utilization (acked over allotted) is logged for contracts opened through the
// manager.
func (self *ContractManager) CloseContractWithCheckpoint(
	contractId Id,
	ackedByteCount ByteCount,
	unackedByteCount ByteCount,
	checkpoint bool,
) {
	opened := false
	var contractKey ContractKey
	var allottedByteCount ByteCount

	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		if allotted, ok := self.localStats.ContractOpenByteCounts[contractId]; ok {
			// opened via the contract manager
			opened = true
			allottedByteCount = allotted
			contractKey = self.localStats.ContractOpenKeys[contractId]
			if !checkpoint {
				self.localStats.ContractCloseCount += 1
				delete(self.localStats.ContractOpenByteCounts, contractId)
				delete(self.localStats.ContractOpenKeys, contractId)
				self.localStats.ContractCloseByteCount += ackedByteCount
			}
		} else {
			self.localStats.ReceiveContractCloseByteCount += ackedByteCount
		}
	}()

	if opened {
		util := 0.0
		if allottedByteCount > 0 {
			util = float64(ackedByteCount) / float64(allottedByteCount) * 100
		}
		action := "closed"
		if checkpoint {
			action = "checkpointed"
		}
		self.client.log.Infof("[contract] %s acked=%s allotted=%s util=%.0f%% destination=%s\n",
			action,
			ByteCountHumanReadable(ackedByteCount),
			ByteCountHumanReadable(allottedByteCount),
			util,
			contractKey.Destination.DestinationId)
		if !checkpoint {
			atomic.AddUint64(&contractUtilSum, uint64(util))
		}
	}

	// Unlike the localStats.ContractOpenByteCounts/ContractOpenKeys bookkeeping
	// above (which legitimately stays untouched on checkpoint — the wire-level
	// contract may still be reused by a new sequence), the per-contract stats
	// entry's lifecycle is tied to this sequence's Run() ending, not to the
	// wire contract's lifecycle. It must always be released here, checkpoint
	// or not, or it leaks for every contract whose sequence ends via
	// checkpoint instead of a hard close — which per ReceiveSequence.Run's
	// defer is the common case, not the exception.
	self.closeContractStats(contractId)

	// Reliable delivery via a per-contract `ControlSync`. The
	// previous implementation called `ClientOob().SendControl(...)`
	// once and dropped the result on the floor — a single transient
	// transport failure would leave the contract `open=true` on the
	// server, its escrow permanently deducted from the network
	// balance with no way for the client to ever signal completion.
	//
	// `ControlSync` retries the send until the platform acks (or
	// the client's context is canceled). One `ControlSync` per
	// close: each contract's close is independent and must not be
	// superseded by another close's `Send` (which is what would
	// happen on a shared `ControlSync` — its `syncCount` would
	// abandon the older close as "replaced"). The per-call instance
	// holds little state — a mutex, a monitor, and a derived context
	// — and its supervisor goroutine exits on success or when the
	// parent context closes, so there's no long-lived leak.
	frame, err := ToFrame(&protocol.CloseContract{
		ContractId:       contractId.Bytes(),
		AckedByteCount:   uint64(ackedByteCount),
		UnackedByteCount: uint64(unackedByteCount),
		Checkpoint:       checkpoint,
	}, self.settings.ProtocolVersion)
	if err != nil {
		self.client.log.Infof("[contract]could not create close contract frame = %s\n", err)
		return
	}
	closeControlSync := NewControlSync(self.ctx, self.client, fmt.Sprintf("close-contract-%s", contractId))
	closeControlSync.Send(frame, nil, func(sendErr error) {
		defer closeControlSync.Close()
		if sendErr == nil && opened {
			contractQueue := self.openContractQueue(contractKey)
			contractQueue.RemoveUsedContract(contractId)
			self.closeContractQueue(contractKey)
		}
	})
}

// LocalStats returns a snapshot of the manager's accumulated contract stats
// (open counts, open byte counts and keys, close byte counts) with the maps
// copied, safe to read while contract activity continues.
func (self *ContractManager) LocalStats() *ContractManagerStats {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return &ContractManagerStats{
		ContractOpenCount:      self.localStats.ContractOpenCount,
		ContractCloseCount:     self.localStats.ContractCloseCount,
		ContractOpenByteCounts: maps.Clone(self.localStats.ContractOpenByteCounts),
		ContractOpenKeys:       maps.Clone(self.localStats.ContractOpenKeys),
		// ContractOpenDestinationIds: maps.Clone(self.localStats.ContractOpenDestinationIds),
		ContractCloseByteCount:        self.localStats.ContractCloseByteCount,
		ReceiveContractCloseByteCount: self.localStats.ReceiveContractCloseByteCount,
	}
}

// ResetLocalStats clears the accumulated local contract stats, starting fresh.
// It is safe to call concurrently with contract activity.
func (self *ContractManager) ResetLocalStats() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.localStats = NewContractManagerStats()
}

// Flush closes every queued, not-yet-taken contract across all destinations
// and returns their ids, dropping destination queues that become done.
// resetUsedContractIds additionally clears each queue's recorded used ids.
// This is the manager-level teardown used on client shutdown.
func (self *ContractManager) Flush(resetUsedContractIds bool) []Id {
	// close queued contracts
	contracts := func() []*protocol.Contract {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		self.client.log.V(1).Infof("[contract]flush %s %s\n", self.client.ClientId(), maps.Keys(self.destinationContracts))

		contracts := []*protocol.Contract{}
		for contractKey, contractQueue := range self.destinationContracts {
			for _, contract := range contractQueue.Flush(resetUsedContractIds) {
				contracts = append(contracts, contract)
			}
			if contractQueue.IsDone() {
				delete(self.destinationContracts, contractKey)
			}
		}
		return contracts
	}()

	return self.closeContracts(contracts)
}

// FlushContractQueue closes the queued contracts for one contract key and
// returns their ids, force-removing the key's queue from the destination map
// even if it is not done. This is the per-sequence exit-flush: it releases the
// escrow of contracts a closing sequence never took. resetUsedContractIds also
// clears the queue's recorded used ids.
func (self *ContractManager) FlushContractQueue(contractKey ContractKey, resetUsedContractIds bool) []Id {
	contractQueue := self.openContractQueue(contractKey)
	defer self.closeContractQueueWithForceRemove(contractKey, true)

	contracts := contractQueue.Flush(resetUsedContractIds)

	return self.closeContracts(contracts)
}

// closeContracts closes each contract with zero acked and unacked byte counts
// and returns their ids. Contracts whose stored bytes do not parse to a
// contract id are skipped.
func (self *ContractManager) closeContracts(contracts []*protocol.Contract) []Id {
	contractIds := []Id{}
	for _, contract := range contracts {
		storedContract := &protocol.StoredContract{}
		if err := ProtoUnmarshal(contract.StoredContractBytes, storedContract); err == nil {
			if contractId, err := IdFromBytes(storedContract.ContractId); err == nil {
				contractIds = append(contractIds, contractId)
				self.CloseContract(contractId, ByteCount(0), ByteCount(0))
			}
		}
	}
	return contractIds
}

// openContractQueue returns the contract queue for the key, creating it in the
// destination map if absent, and opens a reference on it (incrementing its
// open count) so the queue is not considered done while the caller holds it.
// Each call must be paired with closeContractQueue (or the force-remove
// variant).
func (self *ContractManager) openContractQueue(contractKey ContractKey) *contractQueue {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	contractQueue, ok := self.destinationContracts[contractKey]
	if !ok {
		contractQueue = newContractQueue(self.client.log, false)
		self.destinationContracts[contractKey] = contractQueue
	}
	contractQueue.Open()

	return contractQueue
}

// closeContractQueue releases a reference on the key's contract queue and
// removes the queue from the destination map when it is done (no open
// references, no queued or used contracts). Pair with openContractQueue.
func (self *ContractManager) closeContractQueue(contractKey ContractKey) {
	self.closeContractQueueWithForceRemove(contractKey, false)
}

// closeContractQueueWithForceRemove releases a reference on the key's contract
// queue and, when forceRemove is set, removes it from the destination map
// regardless of its done state. Without forceRemove it behaves like
// closeContractQueue.
func (self *ContractManager) closeContractQueueWithForceRemove(contractKey ContractKey, forceRemove bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	contractQueue, ok := self.destinationContracts[contractKey]
	if ok {
		contractQueue.Close()
		if contractQueue.IsDone() || forceRemove {
			delete(self.destinationContracts, contractKey)
		}
	}
	// else the contract was already force removed
}

// queuedContract wraps a contract with its arrival time so stale entries
// can be expired (see ContractQueueExpireTimeout).
type queuedContract struct {
	contract    *protocol.Contract
	enqueueTime time.Time
}

type contractQueue struct {
	updateMonitor *Monitor
	log           Logger

	mutex     sync.Mutex
	openCount int
	contracts map[Id]*queuedContract

	// remember all added contract ids
	trackUsedContracts bool
	usedContractIds    map[Id]bool
}

func newContractQueue(log Logger, trackUsedContracts bool) *contractQueue {
	return &contractQueue{
		updateMonitor:      NewMonitor(),
		log:                loggerOrDefault(log),
		openCount:          0,
		contracts:          map[Id]*queuedContract{},
		trackUsedContracts: trackUsedContracts,
		usedContractIds:    map[Id]bool{},
	}
}

// Open marks that a caller holds the queue open, incrementing its open count.
// A queue with a nonzero open count is never considered done.
func (self *contractQueue) Open() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.openCount += 1
}

// Close marks that a caller released the queue, decrementing its open count.
func (self *contractQueue) Close() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.openCount -= 1
}

// Poll returns one queued contract, never one enqueued before
// minEnqueueTime — stale entries are removed and returned as expired for
// the caller to close. A zero minEnqueueTime expires nothing.
func (self *contractQueue) Poll(minEnqueueTime time.Time) (*protocol.Contract, []*protocol.Contract) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	expired := self.expireWithLock(minEnqueueTime)

	// choose arbitrarily
	for contractId, qc := range self.contracts {
		delete(self.contracts, contractId)
		return qc.contract, expired
	}
	return nil, expired
}

// expireWithLock removes and returns all contracts enqueued before minEnqueueTime.
// Must be called with self.mutex held.
func (self *contractQueue) expireWithLock(minEnqueueTime time.Time) []*protocol.Contract {
	if minEnqueueTime.IsZero() {
		return nil
	}
	var expired []*protocol.Contract
	for contractId, qc := range self.contracts {
		if qc.enqueueTime.Before(minEnqueueTime) {
			expired = append(expired, qc.contract)
			delete(self.contracts, contractId)
		}
	}
	return expired
}

// Expire removes and returns all contracts enqueued before minEnqueueTime.
func (self *contractQueue) Expire(minEnqueueTime time.Time) []*protocol.Contract {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.expireWithLock(minEnqueueTime)
}

// Add queues a contract (with its parsed stored contract) for later Poll or
// Flush. It fails only when the stored contract's id cannot be parsed. A
// contract whose id is already queued replaces the queued entry and refreshes
// its enqueue time. When the queue tracks used ids, a contract whose id was
// already recorded as used is dropped instead.
func (self *contractQueue) Add(contract *protocol.Contract, storedContract *protocol.StoredContract) error {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	contractId, err := IdFromBytes(storedContract.ContractId)
	if err != nil {
		return err
	}

	// update contract if present
	if _, ok := self.contracts[contractId]; ok {
		self.log.V(2).Infof("[contract]add update existing %s\n", contractId)
		self.contracts[contractId] = &queuedContract{contract: contract, enqueueTime: time.Now()}
		self.updateMonitor.NotifyAll()
	} else if !self.trackUsedContracts || !self.usedContractIds[contractId] {
		self.log.V(2).Infof("[contract]add %s\n", contractId)
		if self.trackUsedContracts {
			self.usedContractIds[contractId] = true
		}
		self.contracts[contractId] = &queuedContract{contract: contract, enqueueTime: time.Now()}
		self.updateMonitor.NotifyAll()
	} else {
		self.log.V(2).Infof("[contract]add already used %s\n", contractId)
		// drop this contract. it has already been used locally
	}
	return nil
}

// RemoveUsedContract forgets a previously recorded used contract id so a later
// contract with the same id is accepted again. It is called from the close
// path once a contract's close is acknowledged; it has no effect when the
// queue does not track used ids.
func (self *contractQueue) RemoveUsedContract(contractId Id) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	delete(self.usedContractIds, contractId)
}

// Flush removes and returns all queued contracts, emptying the queue. When
// removeUsedContractIds is set, the recorded used ids are cleared as well.
func (self *contractQueue) Flush(removeUsedContractIds bool) []*protocol.Contract {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	contracts := make([]*protocol.Contract, 0, len(self.contracts))
	for _, qc := range self.contracts {
		contracts = append(contracts, qc.contract)
	}
	self.contracts = map[Id]*queuedContract{}
	if removeUsedContractIds {
		self.usedContractIds = map[Id]bool{}
	}

	return contracts
}

// IsDone reports whether the queue can be dropped: no open references, no
// queued contracts, and no recorded used ids.
func (self *contractQueue) IsDone() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if 0 < self.openCount {
		return false
	}

	return 0 == len(self.contracts) && 0 == len(self.usedContractIds)
}

// UsedContractIdBytes returns the ids recorded as used, one byte slice per id.
// Only populated when the queue tracks used ids; the manager's queues are
// created without tracking, so for them this is always empty.
func (self *contractQueue) UsedContractIdBytes() [][]byte {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	usedContractIdBytes := [][]byte{}
	for contractId, _ := range self.usedContractIds {
		usedContractIdBytes = append(usedContractIdBytes, contractId.Bytes())
	}
	return usedContractIdBytes
}
