package connect

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

func init() {
	MessagePoolReturn(MessagePoolGet(1))
}

func expirationTestContract(t *testing.T, source, destination Id, expiration *int64) (*protocol.Contract, *protocol.StoredContract) {
	t.Helper()
	stored := &protocol.StoredContract{
		ContractId:              NewId().Bytes(),
		SourceId:                source.Bytes(),
		DestinationId:           destination.Bytes(),
		TransferByteCount:       uint64(mib(1)),
		ExpirationTimeUnixMilli: expiration,
	}
	wire, err := proto.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	return &protocol.Contract{StoredContractBytes: wire, ProvideMode: protocol.ProvideMode_Network}, stored
}

func expirationTestClientSettings() *ClientSettings {
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.ControlPingTimeout = 0
	settings.Log = NewNoopLogger()
	return settings
}

func expiryQueueCount(manager *ContractManager) int {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	return len(manager.destinationContracts)
}

// 1. TestContractExpirationPresenceAndSignedBoundary
func TestContractExpirationPresenceAndSignedBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UnixMilli()
		cases := []struct {
			name     string
			deadline int64
			absent   bool
			allowed  bool
		}{
			{name: "legacy", absent: true, allowed: true},
			{name: "zero", deadline: 0},
			{name: "negative", deadline: -1},
			{name: "go-zero-time", deadline: time.Time{}.UnixMilli()},
			{name: "minimum", deadline: math.MinInt64},
			{name: "maximum", deadline: math.MaxInt64, allowed: true},
			{name: "before", deadline: now - 1},
			{name: "equal", deadline: now},
			{name: "after", deadline: now + 1, allowed: true},
		}
		for _, testCase := range cases {
			var deadline *int64
			if !testCase.absent {
				deadline = &testCase.deadline
			}
			wire, stored := expirationTestContract(t, NewId(), NewId(), deadline)
			contract, err := newSequenceContract(NewNoopLogger(), "s", wire, 0, 1)
			if err != nil {
				t.Fatal(err)
			}
			if got := contract.update(10); got != testCase.allowed {
				t.Errorf("%s update=%t want=%t", testCase.name, got, testCase.allowed)
			}
			queue := newContractQueue(NewNoopLogger(), false)
			if err := queue.Add(wire, stored); err != nil {
				t.Fatal(err)
			}
			taken, expired := queue.Poll(time.Time{})
			wantExpired := 1
			if testCase.allowed {
				wantExpired = 0
			}
			if (taken != nil) != testCase.allowed || len(expired) != wantExpired {
				t.Errorf("%s queue admitted=%t expired=%d", testCase.name, taken != nil, len(expired))
			}
			if !testCase.allowed && (contract.ackedByteCount != 0 || contract.unackedByteCount != 0) {
				t.Errorf("%s rejection changed accounting", testCase.name)
			}
		}
	})
}

// 2. TestContractExpirationProtoRoundTrip
func TestContractExpirationProtoRoundTrip(t *testing.T) {
	// Without field 12 (legacy / nil)
	wireNil, _ := expirationTestContract(t, NewId(), NewId(), nil)
	cNil, err := newSequenceContract(NewNoopLogger(), "s", wireNil, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if cNil.expirationTimeUnixMilli != nil {
		t.Fatalf("expected nil expirationTimeUnixMilli, got %v", *cNil.expirationTimeUnixMilli)
	}

	// With field 12 explicitly set to 0
	valZero := int64(0)
	wireZero, _ := expirationTestContract(t, NewId(), NewId(), &valZero)
	cZero, err := newSequenceContract(NewNoopLogger(), "s", wireZero, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if cZero.expirationTimeUnixMilli == nil {
		t.Fatal("expected non-nil expirationTimeUnixMilli for &0, got nil")
	}
	if *cZero.expirationTimeUnixMilli != 0 {
		t.Fatalf("expected 0, got %d", *cZero.expirationTimeUnixMilli)
	}

	// Verify copying value, not aliasing pointer
	storedVal := int64(12345)
	wireVal, stored := expirationTestContract(t, NewId(), NewId(), &storedVal)
	cVal, err := newSequenceContract(NewNoopLogger(), "s", wireVal, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if cVal.expirationTimeUnixMilli == nil || *cVal.expirationTimeUnixMilli != 12345 {
		t.Fatalf("expected 12345, got %v", cVal.expirationTimeUnixMilli)
	}
	storedValMutated := int64(99999)
	stored.ExpirationTimeUnixMilli = &storedValMutated
	if *cVal.expirationTimeUnixMilli != 12345 {
		t.Fatalf("value was aliased, not copied: got %d, want 12345", *cVal.expirationTimeUnixMilli)
	}

	// Verify unmarshaling round-trip
	var unmarshaled protocol.StoredContract
	if err := proto.Unmarshal(wireZero.StoredContractBytes, &unmarshaled); err != nil {
		t.Fatal(err)
	}
	if unmarshaled.ExpirationTimeUnixMilli == nil || *unmarshaled.ExpirationTimeUnixMilli != 0 {
		t.Fatalf("round-trip proto failed for 0: %v", unmarshaled.ExpirationTimeUnixMilli)
	}
}

// 3. TestContractExpirationQueueReaddCannotExtendDeadline
func TestContractExpirationQueueReaddCannotExtendDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		deadline := time.Now().Add(time.Second).UnixMilli()
		wire, stored := expirationTestContract(t, NewId(), NewId(), &deadline)
		queue := newContractQueue(NewNoopLogger(), true)
		if err := queue.Add(wire, stored); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		if err := queue.Add(wire, stored); err != nil {
			t.Fatal(err)
		}
		if taken, expired := queue.Poll(time.Time{}); taken != nil || len(expired) != 1 || expired[0] != wire {
			t.Fatal("re-enqueue extended the signed deadline with orphan expiry disabled")
		}
	})
}

// 4. TestContractExpirationPollExpiresWithAgeLimitDisabled
func TestContractExpirationPollExpiresWithAgeLimitDisabled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		deadline := time.Now().Add(-time.Second).UnixMilli()
		wire, stored := expirationTestContract(t, NewId(), NewId(), &deadline)
		queue := newContractQueue(NewNoopLogger(), false)
		if err := queue.Add(wire, stored); err != nil {
			t.Fatal(err)
		}
		taken, expired := queue.Poll(time.Time{})
		if taken != nil || len(expired) != 1 || expired[0] != wire {
			t.Fatalf("Poll(time.Time{}) with expired contract: taken=%v, expired=%d, want nil and 1", taken, len(expired))
		}
	})
}

// 5. TestContractExpirationIdleQueueExpiresWithAgeLimitDisabled
func TestContractExpirationIdleQueueExpiresWithAgeLimitDisabled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := expirationTestClientSettings()
		settings.ContractManagerSettings.ContractQueueExpireTimeout = 0
		client := NewClient(t.Context(), ControlId, NewNoContractClientOob(), settings)
		defer client.Close()
		manager := client.ContractManager()
		key := ContractKey{Destination: DestinationId(NewId())}
		deadline := time.Now().Add(time.Minute).UnixMilli()
		wire, _ := expirationTestContract(t, client.ClientId(), key.Destination.DestinationId, &deadline)
		if err := manager.addContract(key, wire); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		if count := expiryQueueCount(manager); count != 0 {
			t.Fatalf("idle expired grant retained %d queues", count)
		}
	})
}

// 6. TestContractExpirationQueueMinimumCleanupInterval
func TestContractExpirationQueueMinimumCleanupInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := expirationTestClientSettings()
		settings.ContractManagerSettings.ContractQueueExpireTimeout = time.Nanosecond
		client := NewClient(t.Context(), ControlId, NewNoContractClientOob(), settings)
		defer client.Close()
		manager := client.ContractManager()
		key := ContractKey{Destination: DestinationId(NewId())}
		wire, _ := expirationTestContract(t, client.ClientId(), key.Destination.DestinationId, nil)
		if err := manager.addContract(key, wire); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		time.Sleep(3 * time.Nanosecond)
		synctest.Wait()
		if count := expiryQueueCount(manager); count != 0 {
			t.Fatalf("minimum interval did not remove orphan queue: %d", count)
		}
	})
}

// 7. TestContractExpirationSendRotatesWithoutWaiting
func TestContractExpirationSendRotatesWithoutWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clientSettings := DefaultClientSettings()
		clientSettings.EncryptionSettings.Mode = EncryptionModeOff
		clientSettings.SendBufferSettings.MinResendInterval = 5 * time.Millisecond
		clientSettings.SendBufferSettings.MaxResendInterval = 10 * time.Millisecond
		clientSettings.SendBufferSettings.AckTimeout = 10 * time.Millisecond
		clientSettings.SendBufferSettings.IdleTimeout = time.Hour
		clientSettings.SendBufferSettings.CreateContractTimeout = 5 * time.Second

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
		defer client.Close()
		route := make(chan []byte, 1024)
		gatewayTransport := NewSendGatewayTransport()
		client.RouteManager().UpdateTransport(gatewayTransport, []Route{route})
		sendBuffer := NewSendBuffer(ctx, client, clientSettings.SendBufferSettings)
		destination := DestinationId(NewId())
		seq := NewSendSequence(
			ctx,
			client,
			sendBuffer,
			destination,
			MultiHopId{},
			false,
			true,
			sequenceTlsRoleClient,
			false,
			clientSettings.SendBufferSettings,
		)
		client.ContractManager().mutex.Lock()
		client.ContractManager().sendNoContractClientIds[destination.DestinationId] = false
		client.ContractManager().mutex.Unlock()

		deadline := time.Now().Add(time.Second).UnixMilli()
		currentWire, _ := expirationTestContract(t, client.ClientId(), destination.DestinationId, &deadline)
		currentContract, err := newSequenceContract(
			client.log,
			"s",
			currentWire,
			clientSettings.SendBufferSettings.MinMessageByteCount,
			seq.contractFillFraction(),
		)
		if err != nil {
			t.Fatal(err)
		}
		if !currentContract.update(100) {
			t.Fatal("initial debit failed")
		}
		seq.openSendContracts[currentContract.contractId] = currentContract
		seq.sendContract = currentContract

		successorDeadline := time.Now().Add(time.Hour).UnixMilli()
		successorWire, _ := expirationTestContract(t, client.ClientId(), destination.DestinationId, &successorDeadline)
		contractKey := ContractKey{
			Destination:         seq.destination,
			IntermediaryIds:     seq.intermediaryIds,
			CompanionContract:   seq.companionContract,
			ForceStream:         seq.forceStream,
			EncryptionRole:      seq.encryptionRole,
			EncryptionCompanion: seq.encryptionCompanion,
		}
		if err := client.ContractManager().addContract(contractKey, successorWire); err != nil {
			t.Fatal(err)
		}

		time.Sleep(time.Second)

		start := time.Now()
		if !seq.updateContract(50) {
			t.Fatal("updateContract failed to switch to queued successor")
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("updateContract waited %v, expected zero virtual elapsed", elapsed)
		}

		if seq.sendContract == currentContract {
			t.Fatal("sendContract did not rotate to successor")
		}
		if seq.openSendContracts[currentContract.contractId] != currentContract {
			t.Fatal("old contract was removed from openSendContracts while holding unacked bytes")
		}
		if currentContract.unackedByteCount != 100 {
			t.Fatalf("old contract unacked bytes changed: got %d, want 100", currentContract.unackedByteCount)
		}

		ackItem := &sendItem{
			transferItem: transferItem{
				messageId:        NewId(),
				sequenceNumber:   0,
				messageByteCount: 100,
			},
		}
		oldId := currentContract.contractId
		ackItem.contractId = &oldId
		seq.ackItem(ackItem)

		if currentContract.ackedByteCount != 100 || currentContract.unackedByteCount != 0 {
			t.Fatalf("late ack did not settle old contract: acked=%d, unacked=%d", currentContract.ackedByteCount, currentContract.unackedByteCount)
		}
		if seq.openSendContracts[currentContract.contractId] != nil {
			t.Fatal("old contract was not closed and removed from openSendContracts after settlement")
		}
	})
}

// 8. TestContractExpirationReceiverRejectsExpiredRegistrationWithoutAudit
func TestContractExpirationReceiverRejectsExpiredRegistrationWithoutAudit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		settings := DefaultClientSettings()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.Log = NewNoopLogger()
		client := NewClient(ctx, ControlId, NewNoContractClientOob(), settings)
		defer client.Close()

		client.ContractManager().SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
		})

		rawSourceId := NewId()
		sourcePath := SourceId(rawSourceId)
		receiveSettings := DefaultReceiveBufferSettings()
		seq := NewReceiveSequence(
			ctx,
			client,
			sourcePath,
			NewId(),
			sequenceTlsRoleServer,
			false,
			receiveSettings,
		)
		seq.peerAudit = NewSequencePeerAudit(client, sourcePath, 0)

		secret, ok := client.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		if !ok {
			t.Fatal("failed to get provide secret key")
		}

		expiredDeadline := time.Now().Add(-time.Second).UnixMilli()
		id := NewId()
		storedContract := &protocol.StoredContract{
			ContractId:              id.Bytes(),
			TransferByteCount:       uint64(mib(1)),
			SourceId:                rawSourceId.Bytes(),
			DestinationId:           client.ClientId().Bytes(),
			ExpirationTimeUnixMilli: &expiredDeadline,
		}
		storedBytes, err := proto.Marshal(storedContract)
		if err != nil {
			t.Fatal(err)
		}

		mac := hmac.New(sha256.New, secret)
		macSum := mac.Sum(storedBytes)

		contractProto := &protocol.Contract{
			StoredContractBytes: storedBytes,
			StoredContractHmac:  macSum,
			ProvideMode:         protocol.ProvideMode_Network,
		}
		contractFrameBytes, err := proto.Marshal(contractProto)
		if err != nil {
			t.Fatal(err)
		}

		frame := &protocol.Frame{
			MessageType:  protocol.MessageType_TransferContract,
			MessageBytes: contractFrameBytes,
		}

		badContractBefore := 0
		if seq.peerAudit.peerAudit != nil {
			badContractBefore = seq.peerAudit.peerAudit.BadContractCount
		}

		err = seq.registerContracts(&receiveItem{contractFrame: frame})
		if !errors.Is(err, errContractExpired) {
			t.Fatalf("expected errContractExpired, got %v", err)
		}
		if seq.receiveContract != nil {
			t.Fatal("expired contract was set as receiveContract")
		}
		if len(seq.openReceiveContracts) != 0 {
			t.Fatalf("expected 0 open receive contracts, got %d", len(seq.openReceiveContracts))
		}

		badContractAfter := 0
		if seq.peerAudit.peerAudit != nil {
			badContractAfter = seq.peerAudit.peerAudit.BadContractCount
		}
		if badContractAfter != badContractBefore {
			t.Fatalf("badContractCount changed: was %d, now %d (audited as trust fault)", badContractBefore, badContractAfter)
		}
	})
}

// 9. TestContractExpirationReceiverStopsDebitButAllowsSettlement
func TestContractExpirationReceiverStopsDebitButAllowsSettlement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		settings := DefaultClientSettings()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.Log = NewNoopLogger()
		client := NewClient(ctx, ControlId, NewNoContractClientOob(), settings)
		defer client.Close()

		rawSourceId := NewId()
		sourcePath := SourceId(rawSourceId)
		seq := NewReceiveSequence(
			ctx,
			client,
			sourcePath,
			NewId(),
			sequenceTlsRoleServer,
			false,
			DefaultReceiveBufferSettings(),
		)

		deadline := time.Now().Add(time.Second).UnixMilli()
		wire, _ := expirationTestContract(t, rawSourceId, client.ClientId(), &deadline)
		contract, err := newSequenceContract(
			client.log,
			"r",
			wire,
			DefaultReceiveBufferSettings().MinMessageByteCount,
			1.0,
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := seq.setContract(contract); err != nil {
			t.Fatal(err)
		}

		if !contract.update(150) {
			t.Fatal("pre-expiry debit failed")
		}
		contract.ack(100)

		time.Sleep(time.Second)

		for _, implicit := range []bool{false, true} {
			item := &receiveItem{transferItem: transferItem{messageByteCount: 10}}
			if !implicit {
				item.contractId = &contract.contractId
			}
			if seq.updateContract(item) {
				t.Fatalf("implicit=%t debited an expired receive contract", implicit)
			}
		}

		if contract.ackedByteCount != 100 || contract.unackedByteCount != 50 {
			t.Fatalf("rejected receive debit changed accounting: acked=%d unacked=%d", contract.ackedByteCount, contract.unackedByteCount)
		}

		contract.ack(50)
		if contract.ackedByteCount != 150 || contract.unackedByteCount != 0 {
			t.Fatalf("expiry prevented settlement of previously admitted receive bytes: acked=%d unacked=%d", contract.ackedByteCount, contract.unackedByteCount)
		}
	})
}

// 10. TestContractExpirationLegacyUnaffected
func TestContractExpirationLegacyUnaffected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wire, _ := expirationTestContract(t, NewId(), NewId(), nil)
		contract, err := newSequenceContract(NewNoopLogger(), "s", wire, 0, 1)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(24 * time.Hour)
		if !contract.update(10) {
			t.Fatal("legacy contract with nil deadline failed update after 24h")
		}
	})
}

type recordingContractOob struct {
	mutex        sync.Mutex
	requestTimes []time.Time
}

func (self *recordingContractOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	for _, frame := range frames {
		isCreate := frame.MessageType == protocol.MessageType_TransferCreateContract
		if !isCreate {
			if msg, err := FromFrame(frame); err == nil {
				_, isCreate = msg.(*protocol.CreateContract)
			}
		}
		if isCreate {
			self.mutex.Lock()
			self.requestTimes = append(self.requestTimes, time.Now())
			self.mutex.Unlock()
		}
	}
	if callback != nil {
		callback(nil, nil)
	}
}

func (self *recordingContractOob) Requests() []time.Time {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	copied := make([]time.Time, len(self.requestTimes))
	copy(copied, self.requestTimes)
	return copied
}

// 11. TestContractExpirationSendCreateIssuedImmediatelyWhenExpired
func TestContractExpirationSendCreateIssuedImmediatelyWhenExpired(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetBackendDegraded()
		defer resetBackendDegraded()

		initialWait := time.Second

		runCase := func(expired bool) time.Duration {
			clientSettings := DefaultClientSettings()
			clientSettings.EncryptionSettings.Mode = EncryptionModeOff
			clientSettings.SendBufferSettings.MinResendInterval = 5 * time.Millisecond
			clientSettings.SendBufferSettings.MaxResendInterval = 10 * time.Millisecond
			clientSettings.SendBufferSettings.AckTimeout = 10 * time.Millisecond
			clientSettings.SendBufferSettings.IdleTimeout = time.Hour
			clientSettings.SendBufferSettings.CreateContractTimeout = 5 * time.Second
			clientSettings.SendBufferSettings.CreateContractRetryInterval = initialWait
			clientSettings.ControlPingTimeout = 0
			clientSettings.Log = NewNoopLogger()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			oob := &recordingContractOob{}
			client := NewClient(ctx, NewId(), oob, clientSettings)
			defer client.Close()

			route := make(chan []byte, 1024)
			gatewayTransport := NewSendGatewayTransport()
			client.RouteManager().UpdateTransport(gatewayTransport, []Route{route})
			sendBuffer := NewSendBuffer(ctx, client, clientSettings.SendBufferSettings)
			destination := DestinationId(NewId())
			seq := NewSendSequence(
				ctx,
				client,
				sendBuffer,
				destination,
				MultiHopId{},
				false,
				true,
				sequenceTlsRoleClient,
				false,
				clientSettings.SendBufferSettings,
			)
			client.ContractManager().mutex.Lock()
			client.ContractManager().sendNoContractClientIds[destination.DestinationId] = false
			client.ContractManager().mutex.Unlock()

			var deadline int64
			if expired {
				deadline = time.Now().Add(-time.Second).UnixMilli()
			} else {
				deadline = time.Now().Add(time.Hour).UnixMilli()
			}
			wire, _ := expirationTestContract(t, client.ClientId(), destination.DestinationId, &deadline)
			currentContract, err := newSequenceContract(
				client.log,
				"s",
				wire,
				clientSettings.SendBufferSettings.MinMessageByteCount,
				seq.contractFillFraction(),
			)
			if err != nil {
				t.Fatal(err)
			}
			if !expired {
				currentContract.unackedByteCount = currentContract.effectiveTransferByteCount
			}
			seq.openSendContracts[currentContract.contractId] = currentContract
			seq.sendContract = currentContract

			start := time.Now()
			if seq.updateContract(50) {
				t.Fatal("updateContract succeeded unexpectedly with no successor contract queued")
			}

			requests := oob.Requests()
			if len(requests) == 0 {
				t.Fatal("expected create-contract request to be issued, got none")
			}
			return requests[0].Sub(start)
		}

		if elapsed := runCase(true); elapsed != 0 {
			t.Fatalf("expired: create-contract request issued after %v, want 0", elapsed)
		}

		synctest.Wait()

		if elapsed := runCase(false); elapsed != initialWait {
			t.Fatalf("control (non-expired): create-contract request issued after %v, want initial wait %v", elapsed, initialWait)
		}
	})
}
