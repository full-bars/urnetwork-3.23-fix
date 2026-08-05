package connect

import (
	"context"
	"testing"
	"time"

	// mathrand "math/rand"
	"crypto/hmac"
	"crypto/sha256"

	"github.com/go-playground/assert/v2"

	// "google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// TestVerify_HMACFormats verifies that ContractManager.Verify() accepts both
// the legacy (pre-July 1) and standard (post-July 1) HMAC formats, and rejects
// a bogus HMAC. This is a regression test for the July 1, 2026 platform
// cutover — providers must accept both formats during the transition.
func TestVerify_HMACFormats(t *testing.T) {
	clientId := NewId()
	settings := DefaultClientSettings()
	client := NewClient(context.Background(), clientId, NewNoContractClientOob(), settings)
	defer client.Cancel()
	contractManager := client.ContractManager()

	contractManager.SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Network: true,
	})

	relationship := protocol.ProvideMode_Network
	provideSecretKey, ok := contractManager.GetProvideSecretKey(relationship)
	assert.Equal(t, true, ok)

	contractBytes := []byte("test-contract-data-for-hmac-verification")

	// Legacy format: mac.Sum(data) appends HMAC to data
	legacyMac := hmac.New(sha256.New, provideSecretKey)
	legacyHmac := legacyMac.Sum(contractBytes)

	// Standard format: mac.Write(data); mac.Sum(nil) returns pure HMAC
	standardMac := hmac.New(sha256.New, provideSecretKey)
	standardMac.Write(contractBytes)
	standardHmac := standardMac.Sum(nil)

	// Bogus HMAC: wrong key
	bogusKey := []byte("wrong-secret-key")
	bogusMac := hmac.New(sha256.New, bogusKey)
	bogusMac.Write(contractBytes)
	bogusHmac := bogusMac.Sum(nil)

	// All three must have different lengths to confirm they're distinct checks
	assert.NotEqual(t, legacyHmac, standardHmac)

	// Legacy format must verify
	assert.Equal(t, true, contractManager.Verify(legacyHmac, contractBytes, relationship))

	// Standard format must verify
	assert.Equal(t, true, contractManager.Verify(standardHmac, contractBytes, relationship))

	// Bogus HMAC must NOT verify
	assert.Equal(t, false, contractManager.Verify(bogusHmac, contractBytes, relationship))

	t.Log("legacy HMAC format: PASS")
	t.Log("standard HMAC format: PASS")
	t.Log("bogus HMAC rejection: PASS")
}

func TestTakeContract(t *testing.T) {
	// in parallel, add contracts, take contracts, and optionally return contract
	// make sure all created contracts get eventually taken

	k := 4
	n := 64
	// contractReturnP := float32(0.5)
	timeout := 30 * time.Second

	ctx := context.Background()
	clientId := NewId()
	settings := DefaultClientSettings()
	client := NewClient(ctx, clientId, NewNoContractClientOob(), settings)
	defer client.Cancel()
	contractManager := client.ContractManager()

	destinationId := NewId()

	contractManager.SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Network: true,
		protocol.ProvideMode_Public:  true,
	})

	contracts := make(chan *protocol.Contract)
	contractTimeout := make(chan struct{}, 1)

	go func() {
		for i := 0; i < k*n; i += 1 {
			contractId := NewId()
			contractByteCount := gib(1)

			relationship := protocol.ProvideMode_Public
			provideSecretKey, ok := contractManager.GetProvideSecretKey(relationship)
			assert.Equal(t, true, ok)

			storedContract := &protocol.StoredContract{
				ContractId:        contractId.Bytes(),
				TransferByteCount: uint64(contractByteCount),
				SourceId:          clientId.Bytes(),
				DestinationId:     destinationId.Bytes(),
			}
			storedContractBytes, err := ProtoMarshal(storedContract)
			assert.Equal(t, nil, err)
			defer MessagePoolReturn(storedContractBytes)
			mac := hmac.New(sha256.New, provideSecretKey)
			storedContractHmac := mac.Sum(storedContractBytes)

			verified := contractManager.Verify(storedContractHmac, storedContractBytes, relationship)
			assert.Equal(t, true, verified)

			result := &protocol.CreateContractResult{
				Contract: &protocol.Contract{
					StoredContractBytes: storedContractBytes,
					StoredContractHmac:  storedContractHmac,
					ProvideMode:         relationship,
				},
			}
			frame, err := ToFrame(result, DefaultProtocolVersion)
			assert.Equal(t, nil, err)

			contractManager.HandleControlFrame(
				ContractKey{
					Destination: DestinationId(destinationId),
				},
				frame,
			)
		}
	}()

	for j := 0; j < k; j += 1 {
		go func() {
			for i := 0; i < n; {

				contractKey := ContractKey{
					Destination: DestinationId(destinationId),
				}
				if contract := contractManager.TakeContract(ctx, contractKey, timeout); contract != nil {
					// if mathrand.Float32() < contractReturnP {
					// 	// put back
					// 	contractManager.ReturnContract(ctx, destinationId, contract)
					// } else {
					select {
					case contracts <- contract:
					case <-time.After(timeout):
						select {
						case contractTimeout <- struct{}{}:
						default:
						}
						return
					}
					i += 1
					// }
				}

			}

		}()
	}

	contractIds := map[Id]bool{}

	for i := 0; i < k*n; i += 1 {
		select {
		case contract := <-contracts:
			var storedContract protocol.StoredContract
			err := ProtoUnmarshal(contract.StoredContractBytes, &storedContract)
			assert.Equal(t, nil, err)

			contractId, err := IdFromBytes(storedContract.ContractId)
			assert.Equal(t, nil, err)

			assert.Equal(t, false, contractIds[contractId])
			contractIds[contractId] = true

		case <-time.After(timeout):
			t.FailNow()
		case <-contractTimeout:
			t.FailNow()
		}
	}

	assert.Equal(t, k*n, len(contractIds))

	// no more
	contractKey := ContractKey{
		Destination: DestinationId(destinationId),
	}
	contract := contractManager.TakeContract(ctx, contractKey, 0)
	assert.Equal(t, nil, contract)

	// all the contracts are accounted for
}

func TestCheckpointContractDoesNotCloseOpenContract(t *testing.T) {
	clientId := NewId()
	settings := DefaultClientSettings()
	client := NewClient(context.Background(), clientId, NewNoContractClientOob(), settings)
	defer client.Cancel()
	cm := client.ContractManager()

	contractId := NewId()
	cm.mutex.Lock()
	initialCloseCount := cm.localStats.ContractCloseCount
	cm.localStats.ContractOpenByteCounts[contractId] = 1024
	cm.localStats.ContractOpenKeys[contractId] = ContractKey{
		Destination: DestinationId(NewId()),
	}
	cm.mutex.Unlock()

	cm.CheckpointContract(contractId, 100, 50)

	cm.mutex.Lock()
	_, stillOpen := cm.localStats.ContractOpenByteCounts[contractId]
	finalCloseCount := cm.localStats.ContractCloseCount
	cm.mutex.Unlock()

	assert.Equal(t, true, stillOpen)
	assert.Equal(t, initialCloseCount, finalCloseCount)
}

// TestContractByteCount_ZeroScaleDoesNotPanic is a regression test for a
// division-by-zero guard: contractByteCount divides by
// ContractTransferByteSeqScale when computing the lerp between the initial
// and standard contract sizes. If that setting is ever misconfigured to 0
// (e.g. a bad profile override), the guard must return a safe fallback
// instead of panicking.
func TestContractByteCount_ZeroScaleDoesNotPanic(t *testing.T) {
	clientId := NewId()
	settings := DefaultClientSettings()
	client := NewClient(context.Background(), clientId, NewNoContractClientOob(), settings)
	defer client.Cancel()
	cm := client.ContractManager()

	cm.settings.ContractTransferByteSeqScale = 0

	var result ByteCount
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("contractByteCount panicked with ContractTransferByteSeqScale=0: %v", r)
			}
		}()
		result = cm.contractByteCount(0, 0)
	}()

	assert.Equal(t, cm.settings.StandardContractTransferByteCount, result)
}

// TestContractByteCount_ZeroScaleRespectsMinByteCount confirms the zero-scale
// fallback still honors minByteCount, matching the non-zero-scale path's
// behavior (both return max(targetByteCount, minByteCount)).
func TestContractByteCount_ZeroScaleRespectsMinByteCount(t *testing.T) {
	clientId := NewId()
	settings := DefaultClientSettings()
	client := NewClient(context.Background(), clientId, NewNoContractClientOob(), settings)
	defer client.Cancel()
	cm := client.ContractManager()

	cm.settings.ContractTransferByteSeqScale = 0
	largeMin := cm.settings.StandardContractTransferByteCount * 10

	result := cm.contractByteCount(0, largeMin)

	assert.Equal(t, largeMin, result)
}

// TestSequencePeerAuditComplete_LogsSendErrorWithoutPanicking is a regression
// test for SequencePeerAudit.Complete()'s SendControl callback, which used to
// silently swallow the error (func(...){}) and now logs it. NoContractClientOob
// always invokes its callback with a non-nil error ("Not supported."), so any
// client built with it (the standard test fixture throughout this file)
// exercises the new error branch on every Complete() call — this confirms
// that branch doesn't panic and that Complete() still resets peerAudit to nil
// as before, i.e. the logging addition didn't change Complete()'s contract.
func TestSequencePeerAuditComplete_LogsSendErrorWithoutPanicking(t *testing.T) {
	clientId := NewId()
	settings := DefaultClientSettings()
	client := NewClient(context.Background(), clientId, NewNoContractClientOob(), settings)
	defer client.Cancel()

	source := DestinationId(NewId())
	audit := NewSequencePeerAudit(client, source, time.Minute)

	audit.Update(func(pa *PeerAudit) {
		pa.SendByteCount += 1024
		pa.SendCount += 1
	})

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Complete() panicked when SendControl's callback received an error: %v", r)
			}
		}()
		audit.Complete()
	}()

	if audit.peerAudit != nil {
		t.Fatal("expected peerAudit to be reset to nil after Complete()")
	}
}

// successClientOob is a minimal OutOfBandControl double that always succeeds,
// invoking the callback synchronously with a nil error and no result frames.
// It is the success-side counterpart to NewNoContractClientOob, which only
// ever fails.
type successClientOob struct{}

func (self *successClientOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	if callback != nil {
		callback(nil, nil)
	}
}

// TestCreateContract_OobFailureRecordsBackendFailure is a regression test for
// the CreateContract change that replaced the inlined
// inlined failure-recording pair with a call to
// noteBackendFailure(): a failed contract OOB round-trip must still register
// as a backend failure through the shared helper.
func TestCreateContract_OobFailureRecordsBackendFailure(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	clientId := NewId()
	settings := DefaultClientSettings()
	client := NewClient(context.Background(), clientId, NewNoContractClientOob(), settings)
	defer client.Cancel()
	contractManager := client.ContractManager()

	contractKey := ContractKey{
		Destination: DestinationId(NewId()),
	}

	contractManager.CreateContract(contractKey, 0, 0)

	if got := backendFails(); got != 1 {
		t.Fatalf("consecutive failures = %d after one failed CreateContract, want 1", got)
	}
	if backendFail.Load().lastNano == 0 {
		t.Fatal("backend failure timestamp not set after a failed CreateContract")
	}
}

// TestCreateContract_OobSuccessClearsBackendFailure is a regression test for
// the CreateContract change that replaced the inlined
// inlined clear pair with a
// call to noteBackendSuccess(): a successful contract OOB round-trip must
// still clear a pre-existing failure streak through the shared helper.
func TestCreateContract_OobSuccessClearsBackendFailure(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	// seed a pre-existing failure streak, as if a prior auth or contract
	// round-trip had failed.
	noteBackendFailure()
	noteBackendFailure()

	clientId := NewId()
	settings := DefaultClientSettings()
	client := NewClient(context.Background(), clientId, &successClientOob{}, settings)
	defer client.Cancel()
	contractManager := client.ContractManager()

	contractKey := ContractKey{
		Destination: DestinationId(NewId()),
	}

	contractManager.CreateContract(contractKey, 0, 0)

	if got := backendFails(); got != 0 {
		t.Fatalf("consecutive failures = %d after a successful CreateContract, want 0", got)
	}
	if backendFail.Load().lastNano != 0 {
		t.Fatal("backend failure timestamp not cleared after a successful CreateContract")
	}
}

// TestCreateContract_ClientDoneSkipsFailureRecording confirms the
// client.Done() carve-out around the noteBackendFailure() call still holds
// after the switch to the helper: a contract OOB error that arrives after
// (or because) the client has closed must not be recorded as a backend
// failure, since it says nothing about the backend's health.
func TestCreateContract_ClientDoneSkipsFailureRecording(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	clientId := NewId()
	settings := DefaultClientSettings()
	client := NewClient(context.Background(), clientId, NewNoContractClientOob(), settings)
	// close the client before the OOB round-trip is attempted, so the error
	// callback observes client.Done() already closed.
	client.Cancel()

	contractKey := ContractKey{
		Destination: DestinationId(NewId()),
	}

	client.ContractManager().CreateContract(contractKey, 0, 0)

	if got := backendFails(); got != 0 {
		t.Fatalf("consecutive failures = %d after CreateContract on a closed client, want 0 (client.Done() carve-out should skip recording)", got)
	}
	if backendFail.Load().lastNano != 0 {
		t.Fatal("backend failure timestamp set after CreateContract on a closed client; the client.Done() carve-out should skip recording")
	}
}
