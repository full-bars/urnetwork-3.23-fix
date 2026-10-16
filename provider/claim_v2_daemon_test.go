//go:build linux || darwin

// E-5 at the unit level: the v2 claim daemon over a synthetic vault2 pool.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
	"gopkg.in/yaml.v3"

	"github.com/urfoundation/sn/merkle"
)

var claimV2TestSecond = [32]byte{0xc0, 0x1d, 0x03}

func claimV2TestDec(alpha float64) string {
	value := claimV2TestRao(alpha)
	return value.Dec()
}

// E-5 threshold claim: claimable at the threshold is claimed once; one rao
// below it is not claimed at all.
func TestClaimV2ClaimsAtThreshold(t *testing.T) {
	for _, test := range []struct {
		name    string
		rao     uint64
		claimed bool
	}{
		{name: "at threshold", rao: ClaimV2DefaultThresholdRao, claimed: true},
		{name: "below threshold", rao: ClaimV2DefaultThresholdRao - 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			chain := newClaimV2TestChain(t)
			chain.finalize(claimV2TestAccount(claimV2TestOther, 1), merkle.LedgerAccount{Coldkey: claimV2TestColdkey, Cumulative: *uint256.NewInt(test.rao)})
			daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
			daemon.step(false)
			daemon.advance(time.Hour)
			daemon.step(false)
			unit, claimed, sends := daemon.durable(1), chain.claimedOf(1), chain.sent()
			if !test.claimed {
				if len(sends) != 0 || !claimed.IsZero() || unit.Status != "idle" || unit.Claimed != "0" {
					t.Fatalf("below-threshold account was claimed: sends=%d unit=%+v", len(sends), unit)
				}
				return
			}
			want := fmt.Sprint(test.rao)
			if len(sends) != 1 || claimed.Dec() != want || unit.Status != "idle" || unit.Claimed != want || unit.Cumulative != want || unit.RootIndex != 0 || unit.Attempts != 0 || unit.TxHash != "" || unit.RawTxHex != "" || unit.LastError != "" {
				t.Fatalf("threshold claim: sends=%d claimed=%s unit=%+v", len(sends), claimed.Dec(), unit)
			}
			if unit.Coldkey != "0x"+fmt.Sprintf("%x", claimV2TestColdkey) || unit.Vault != daemon.cfg.Vault || unit.NoID != chain.noID {
				t.Fatalf("unit identity = %+v", unit)
			}
		})
	}
}

// E-5 reopen on a newer root: a paid unit reopens when a newer finalized root
// raises the account by at least the threshold, and pays only the increment.
func TestClaimV2ReopensOnNewerRoot(t *testing.T) {
	chain := newClaimV2TestChain(t)
	chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
	daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
	daemon.step(false)
	if unit := daemon.durable(1); unit.Status != "idle" || unit.Claimed != claimV2TestDec(5) {
		t.Fatalf("first claim: %+v", unit)
	}
	chain.finalize(claimV2TestAccount(claimV2TestOther, 2), claimV2TestAccount(claimV2TestColdkey, 8))
	daemon.step(false)
	unit, claimed, sends := daemon.durable(1), chain.claimedOf(1), chain.sent()
	if len(sends) != 2 || claimed.Dec() != claimV2TestDec(8) || unit.Status != "idle" || unit.Claimed != claimV2TestDec(8) || unit.RootIndex != 1 {
		t.Fatalf("reopened claim: sends=%d claimed=%s unit=%+v", len(sends), claimed.Dec(), unit)
	}
	if claimV2TestNonce(t, sends[1]) != claimV2TestNonce(t, sends[0])+1 {
		t.Fatal("reopened claim did not take the next nonce")
	}
	// A rise below the threshold leaves the paid unit idle.
	chain.finalize(claimV2TestAccount(claimV2TestOther, 3), claimV2TestAccount(claimV2TestColdkey, 9))
	daemon.step(false)
	if unit := daemon.durable(1); len(chain.sent()) != 2 || unit.Status != "idle" || unit.Claimed != claimV2TestDec(8) {
		t.Fatalf("sub-threshold rise reopened the unit: %+v", unit)
	}
}

// E-5 a newer root during an in-flight claim: no second claim is signed while
// the first is in flight; once it settles the increment is claimed.
func TestClaimV2NewerRootDuringInFlightClaim(t *testing.T) {
	chain := newClaimV2TestChain(t)
	chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
	daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
	chain.with(func() { chain.sendFailures = 1 })
	daemon.step(false)
	first := daemon.durable(1)
	if first.Status != "uncertain" || first.RawTxHex == "" || !slices.Equal(chain.sent(), []string{first.RawTxHex}) || !strings.Contains(first.LastError, "prepared but finality was not confirmed") {
		t.Fatalf("lost broadcast did not leave one claim in flight: %+v", first)
	}
	chain.finalize(claimV2TestAccount(claimV2TestOther, 2), claimV2TestAccount(claimV2TestColdkey, 8))
	daemon.step(false)
	daemon.advance(time.Minute)
	daemon.step(false)
	if unit := daemon.durable(1); unit.Status != "uncertain" || unit.TxHash != first.TxHash || unit.RawTxHex != first.RawTxHex {
		t.Fatalf("newer root disturbed the in-flight claim: %+v", unit)
	}
	daemon.advance(time.Minute)
	daemon.step(false)
	if unit := daemon.durable(1); unit.Status != "idle" || unit.Claimed != claimV2TestDec(5) || unit.RootIndex != 0 {
		t.Fatalf("in-flight claim did not settle on its own root: %+v", unit)
	}
	daemon.step(false)
	unit, claimed, sends := daemon.durable(1), chain.claimedOf(1), chain.sent()
	if len(sends) != 3 || sends[0] != first.RawTxHex || sends[1] != first.RawTxHex || sends[2] == first.RawTxHex || claimV2TestNonce(t, sends[2]) != claimV2TestNonce(t, first.RawTxHex)+1 {
		t.Fatalf("claims while in flight: %v", sends)
	}
	if claimed.Dec() != claimV2TestDec(8) || unit.Status != "idle" || unit.Claimed != claimV2TestDec(8) || unit.RootIndex != 1 {
		t.Fatalf("increment after settlement: claimed=%s unit=%+v", claimed.Dec(), unit)
	}
}

// E-5 a newer root ready beside a poll tick: as in v1, the loop observes the
// waiting snapshot before it polls, so the claim pays the newer root's
// cumulative at once and no catch-up claim follows.
func TestClaimV2NewerSnapshotOutranksReadyPoll(t *testing.T) {
	chain := newClaimV2TestChain(t)
	chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
	daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
	older, err := readClaimSnapshotV2(daemon.ctx, daemon.strategy, daemon.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.observe(older, false); err != nil {
		t.Fatal(err)
	}
	chain.finalize(claimV2TestAccount(claimV2TestOther, 2), claimV2TestAccount(claimV2TestColdkey, 8))
	newer, err := readClaimSnapshotV2(daemon.ctx, daemon.strategy, daemon.cfg)
	if err != nil {
		t.Fatal(err)
	}
	tick := daemon.clock
	snapshots, polls := make(chan map[string]*claimAccountV2, 1), make(chan time.Time, 1)
	snapshots <- newer
	polls <- tick
	ctx, cancel := context.WithCancel(daemon.ctx)
	defer cancel()
	var runErr error
	done := make(chan struct{})
	go func() { defer close(done); runErr = daemon.run(ctx, snapshots, polls, false) }()
	// The loop takes a tick only at its select, so once the second of these is
	// queued the poll after the claim has finished.
	for range 2 {
		select {
		case polls <- tick:
		case <-done:
			t.Fatalf("daemon stopped early: %v", runErr)
		}
	}
	cancel()
	<-done
	unit, claimed, sends := daemon.durable(1), chain.claimedOf(1), chain.sent()
	if runErr != nil || len(sends) != 1 || claimed.Dec() != claimV2TestDec(8) || unit.Status != "idle" || unit.Claimed != claimV2TestDec(8) || unit.RootIndex != 1 {
		t.Fatalf("claim was not built on the newer root alone: err=%v sends=%d claimed=%s unit=%+v", runErr, len(sends), claimed.Dec(), unit)
	}
}

// E-5 a third party claiming first (A6): seen before we sign, refusing our
// preflight, or reverting our included claim with NothingToClaim. Each is done
// for that root at once, with the watermark re-read and no second claim, and
// a newer root reopens the unit for the increment.
func TestClaimV2ThirdPartyClaimsFirst(t *testing.T) {
	for _, test := range []struct {
		name   string
		race   func(*claimV2TestChain)
		sends  int
		settle bool
	}{
		{name: "before signing", race: func(chain *claimV2TestChain) {
			chain.claimed[1] = claimV2TestRao(5)
			chain.account = func(entry map[string]any) { entry["claimed"] = "0" }
		}},
		{name: "at our preflight", race: func(chain *claimV2TestChain) {
			chain.beforePreflight = func() { chain.claimed[1], chain.beforePreflight = claimV2TestRao(5), nil }
		}},
		{name: "before our inclusion", sends: 1, settle: true, race: func(chain *claimV2TestChain) {
			chain.beforeMine = func() { chain.claimed[1], chain.beforeMine = claimV2TestRao(5), nil }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			chain := newClaimV2TestChain(t)
			chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
			chain.with(func() { test.race(chain) })
			daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
			daemon.step(false)
			if test.settle {
				if unit := daemon.durable(1); unit.Status != "uncertain" || unit.RawTxHex == "" {
					t.Fatalf("our raced claim is not in flight: %+v", unit)
				}
				daemon.advance(time.Minute)
				daemon.step(false)
			}
			if unit := daemon.durable(1); len(chain.sent()) != test.sends || unit.Status != "idle" || unit.Claimed != claimV2TestDec(5) || unit.Cumulative != claimV2TestDec(5) || unit.Attempts != 0 || unit.TxHash != "" || unit.LastError != "" || unit.NextRetryAt != "" {
				t.Fatalf("third-party claim was not done for its root: sends=%d %+v", len(chain.sent()), unit)
			}
			chain.with(func() { chain.account = nil })
			chain.finalize(claimV2TestAccount(claimV2TestOther, 2), claimV2TestAccount(claimV2TestColdkey, 8))
			daemon.step(false)
			claimed := chain.claimedOf(1)
			if unit := daemon.durable(1); len(chain.sent()) != test.sends+1 || claimed.Dec() != claimV2TestDec(8) || unit.Status != "idle" || unit.Claimed != claimV2TestDec(8) || unit.RootIndex != 1 {
				t.Fatalf("a newer root did not reopen the unit: sends=%d %+v", len(chain.sent()), unit)
			}
		})
	}
}

// E-5 v1 and v2 side by side: one process runs the unchanged v1 daemon and
// the v2 daemon on one relayer key. Each keeps its own queue schema, and the
// v2 signature takes the nonce above v1's retained one from the shared floor.
func TestClaimV2RunsBesideV1(t *testing.T) {
	v1, _, signed, receipt, block := signedClaimFixture(t, 70, 23)
	v1.SchemaVersion, v1.Release, v1.PollSeconds, v1.LookbackEpochs = 1, "1.0", 5, 2
	v1.RPC = []string{claimReceiptIdentityTestRPC(t, receipt, block)}
	state := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", state)
	if err := os.WriteFile(filepath.Join(state, "jwt"), []byte(financialTestJwt(t, "", "legacy-bootstrap")), 0o600); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/sn/epoch" {
			_ = json.NewEncoder(writer).Encode(map[string]int{"epoch": 72})
		}
	}))
	t.Cleanup(api.Close)
	v1.APIURL = api.URL
	chain := newClaimV2TestChain(t)
	chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
	// v1's nonces 0 to 23 are mined, but this endpoint's nonce view lags at 0.
	lagging := uint64(0)
	chain.with(func() { chain.nonce, chain.nonceView = 24, &lagging })
	v2Dir := filepath.Join(filepath.Dir(v1.StateDir), "claims-v2")
	v2Path := writeClaimV2TestConfig(t, chain, v1.KeyFile, v2Dir, claimV2TestColdkey)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	ctx = claimQueueTestContext(t, ctx, v1.StateDir, v2Dir)
	store, err := newClaimQueueStore(v1.StateDir, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.save(&ClaimQueue{Schema: "urnetwork-provider-claim-queue-v1", LastDiscovered: 70, Entries: map[string]*ClaimQueueEntry{"70": signed}}); err != nil {
		t.Fatal(err)
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	raw, err := yaml.Marshal(v1)
	if err != nil {
		t.Fatal(err)
	}
	v1Path := filepath.Join(t.TempDir(), "claim.yml")
	if err := os.WriteFile(v1Path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- RunClaimDaemonV2(ctx, v2Path, v1Path, ClaimV2DefaultThresholdRao, false) }()
	var v1Queue ClaimQueue
	for {
		raw, err := os.ReadFile(filepath.Join(v1.StateDir, "claim-queue.json"))
		if unit := claimV2TestQueued(v2Dir, 1); err == nil && json.Unmarshal(raw, &v1Queue) == nil && v1Queue.Entries["70"].Status == "finalized" && unit != nil && unit.Status == "idle" {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("v1 and v2 daemons did not both settle")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	sends, unit := chain.sent(), claimV2TestQueued(v2Dir, 1)
	if len(sends) != 1 || claimV2TestNonce(t, sends[0]) != 24 || unit.Claimed != claimV2TestDec(5) {
		t.Fatalf("v2 beside v1: sends=%d unit=%+v", len(sends), unit)
	}
	if entry := v1Queue.Entries["70"]; v1Queue.Schema != "urnetwork-provider-claim-queue-v1" || entry.TxHash != signed.TxHash || entry.RawTxHex != signed.RawTxHex {
		t.Fatalf("v1 queue changed beside v2: %+v", entry)
	}
	// Each daemon refuses the other's queue.
	refused := claimQueueTestContext(t, t.Context(), v1.StateDir, v2Dir)
	v2Store, err := newClaimQueueV2Store(refused, v1.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v2Store.load(); err == nil || !strings.Contains(err.Error(), "invalid v2 claim queue schema") {
		t.Fatalf("v2 daemon accepted a v1 queue: %v", err)
	}
	v1Store, err := newClaimQueueStore(v2Dir, refused)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v1Store.load(); err == nil || !strings.Contains(err.Error(), "invalid claim queue schema") {
		t.Fatalf("v1 daemon accepted a v2 queue: %v", err)
	}
	if err := errors.Join(v2Store.close(), v1Store.close()); err != nil {
		t.Fatal(err)
	}
}

// E-5 restart mid-claim: after a lost broadcast, at the prepared checkpoint,
// and before it. Retained bytes are reconciled and never re-signed; without
// them the claim is simply retried.
func TestClaimV2RestartMidClaim(t *testing.T) {
	t.Run("after a lost broadcast", func(t *testing.T) {
		chain := newClaimV2TestChain(t)
		chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
		daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
		chain.with(func() { chain.sendFailures = 1 })
		daemon.step(false)
		first := daemon.durable(1)
		daemon.restart()
		if daemon.admission.nonceMinimum() != claimV2TestNonce(t, first.RawTxHex)+1 {
			t.Fatal("restart did not seed the retained nonce")
		}
		daemon.advance(time.Minute)
		daemon.step(false)
		daemon.advance(time.Minute)
		daemon.step(false)
		if unit := daemon.durable(1); !slices.Equal(chain.sent(), []string{first.RawTxHex, first.RawTxHex}) || unit.Status != "idle" || unit.Claimed != claimV2TestDec(5) {
			t.Fatalf("restart did not reconcile the exact claim: %+v", unit)
		}
	})
	t.Run("at the prepared checkpoint", func(t *testing.T) {
		chain := newClaimV2TestChain(t)
		chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
		daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
		hash, raw := daemon.sign(daemon.account(1), 0)
		key := claimUnitV2Key(daemon.cfg.Vault, chain.noID, 1)
		daemon.queue.Units[key] = &ClaimUnitV2{Vault: daemon.cfg.Vault, NoID: chain.noID, Index: 1, Status: "submitting", Attempts: 1, Cumulative: "0", Claimed: "0", TxHash: hash, RawTxHex: raw}
		if err := daemon.store.save(daemon.queue); err != nil {
			t.Fatal(err)
		}
		daemon.restart()
		if unit := daemon.unit(1); unit.Status != "uncertain" || unit.LastError != "process restarted after exact transaction preparation" || daemon.admission.nonceMinimum() != 1 {
			t.Fatalf("prepared restart: %+v", unit)
		}
		daemon.step(false)
		daemon.advance(time.Minute)
		daemon.step(false)
		if unit := daemon.durable(1); !slices.Equal(chain.sent(), []string{raw}) || unit.Status != "idle" || unit.Claimed != claimV2TestDec(5) {
			t.Fatalf("prepared bytes were not the only claim: %+v", unit)
		}
	})
	t.Run("before the prepared checkpoint", func(t *testing.T) {
		chain := newClaimV2TestChain(t)
		chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
		daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
		key := claimUnitV2Key(daemon.cfg.Vault, chain.noID, 1)
		daemon.queue.Units[key] = &ClaimUnitV2{Vault: daemon.cfg.Vault, NoID: chain.noID, Index: 1, Status: "submitting", Attempts: 1, Cumulative: "0", Claimed: "0"}
		if err := daemon.store.save(daemon.queue); err != nil {
			t.Fatal(err)
		}
		daemon.restart()
		if unit := daemon.unit(1); unit.Status != "open" || unit.LastError != "process restarted before the durable broadcast boundary" {
			t.Fatalf("unsigned restart: %+v", unit)
		}
		daemon.step(false)
		if unit := daemon.durable(1); len(chain.sent()) != 1 || unit.Status != "idle" || unit.Claimed != claimV2TestDec(5) {
			t.Fatalf("unsigned restart was not retried: %+v", unit)
		}
	})
}

// E-5 one claim in flight per account: while an account's claim is in flight,
// demands and newer roots sign nothing more for it. Another account still
// gets its own claim, at the next nonce.
func TestClaimV2OneClaimInFlightPerAccount(t *testing.T) {
	chain := newClaimV2TestChain(t)
	chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5), claimV2TestAccount(claimV2TestSecond, 6))
	daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao, claimV2TestColdkey, claimV2TestSecond)
	chain.with(func() { chain.sendFailures = 1 })
	daemon.step(false)
	daemon.step(false)
	first, second := daemon.durable(1), daemon.durable(2)
	if first.Status != "uncertain" || second.Status != "uncertain" || second.RawTxHex == first.RawTxHex || claimV2TestNonce(t, second.RawTxHex) != claimV2TestNonce(t, first.RawTxHex)+1 {
		t.Fatalf("each account did not get its own claim: %+v %+v", first, second)
	}
	chain.finalize(claimV2TestAccount(claimV2TestOther, 2), claimV2TestAccount(claimV2TestColdkey, 9), claimV2TestAccount(claimV2TestSecond, 6))
	chain.with(func() { chain.sendFailures = 1_000 })
	for range 6 {
		daemon.advance(time.Minute)
		daemon.step(true)
	}
	for _, raw := range chain.sent() {
		if raw != first.RawTxHex && raw != second.RawTxHex {
			t.Fatal("a second claim was signed for an account with one in flight")
		}
	}
	if unit := daemon.durable(1); unit.Status != "uncertain" || unit.TxHash != first.TxHash || unit.RawTxHex != first.RawTxHex || !unit.Demand {
		t.Fatalf("in-flight unit: %+v", unit)
	}
	chain.with(func() { chain.sendFailures = 0 })
	for range 10 {
		daemon.advance(time.Minute)
		daemon.step(false)
	}
	claims := []string{}
	for _, raw := range chain.sent() {
		if !slices.Contains(claims, raw) {
			claims = append(claims, raw)
		}
	}
	claimedFirst, claimedSecond := chain.claimedOf(1), chain.claimedOf(2)
	if len(claims) != 3 || claims[0] != first.RawTxHex || claims[1] != second.RawTxHex || claimV2TestNonce(t, claims[2]) != 2 || claimedFirst.Dec() != claimV2TestDec(9) || claimedSecond.Dec() != claimV2TestDec(6) {
		t.Fatalf("claims after settlement: %d, claimed %s and %s", len(claims), claimedFirst.Dec(), claimedSecond.Dec())
	}
	if unit := daemon.durable(1); unit.Status != "idle" || unit.Claimed != claimV2TestDec(9) || unit.Demand {
		t.Fatalf("settled unit: %+v", unit)
	}
}

// E-5 signed bytes never replaced: a rebroadcast whose finalized preflight is
// refused for any reason but a claim the watermark already covers (here the
// runtime's refusal) keeps today's behaviour. The exact bytes stay in flight,
// unsent, across polls, demands and a restart, as do bytes whose nonce
// another signer consumed. Nothing new is signed.
func TestClaimV2SignedBytesNeverReplaced(t *testing.T) {
	chain := newClaimV2TestChain(t)
	chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
	daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
	chain.with(func() { chain.sendFailures = 1 })
	daemon.step(false)
	first := daemon.durable(1)
	chain.with(func() { chain.refuseClaims = true })
	chain.finalize(claimV2TestAccount(claimV2TestOther, 2), claimV2TestAccount(claimV2TestColdkey, 8))
	for _, restart := range []bool{false, true, false} {
		if restart {
			daemon.restart()
		}
		daemon.advance(time.Minute)
		daemon.step(true)
		if unit := daemon.durable(1); unit.Status != "uncertain" || unit.TxHash != first.TxHash || unit.RawTxHex != first.RawTxHex || !strings.Contains(unit.LastError, "exact rebroadcast did not reconcile outcome") || !strings.Contains(unit.LastError, "runtime transfer refusal") {
			t.Fatalf("refused rebroadcast replaced or dropped the signed bytes: %+v", unit)
		}
	}
	if !slices.Equal(chain.sent(), []string{first.RawTxHex}) {
		t.Fatal("bytes refused at preflight for another reason were sent")
	}
	chain.with(func() { chain.nonce = 1 })
	daemon.advance(time.Minute)
	daemon.step(false)
	if unit := daemon.durable(1); !slices.Equal(chain.sent(), []string{first.RawTxHex}) || unit.RawTxHex != first.RawTxHex || !strings.Contains(unit.LastError, "signed nonce is consumed without an authenticated receipt") {
		t.Fatalf("consumed nonce replaced the signed bytes: %+v", unit)
	}
}

// E-5 and A6, a lost broadcast then a third party's claim: the retained bytes
// fail their preflight only because the finalized watermark covers their
// claim, so the same bytes are sent without it. They revert inside the vault
// and consume their nonce; the unit is done for that root once that receipt
// is final, and the claims behind the nonce gap proceed.
func TestClaimV2CoveredClaimClosesItsNonceGap(t *testing.T) {
	chain := newClaimV2TestChain(t)
	chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5), claimV2TestAccount(claimV2TestSecond, 6))
	daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao, claimV2TestColdkey, claimV2TestSecond)
	chain.with(func() { chain.sendFailures = 1 })
	daemon.step(false)
	daemon.step(false)
	first, second := daemon.durable(1), daemon.durable(2)
	if first.Status != "uncertain" || second.Status != "uncertain" || claimV2TestNonce(t, first.RawTxHex) != 0 || claimV2TestNonce(t, second.RawTxHex) != 1 || !strings.Contains(second.LastError, "nonce too high") {
		t.Fatalf("the second claim is not queued behind the lost one: %+v %+v", first, second)
	}
	chain.with(func() { chain.claimed[1] = claimV2TestRao(5) })
	daemon.advance(time.Minute)
	daemon.step(false)
	sends := chain.sent()
	var reverted bool
	chain.with(func() {
		receipt := chain.receipts[common.HexToHash(first.TxHash)]
		reverted = receipt != nil && receipt.Status == types.ReceiptStatusFailed && chain.nonce == 1
	})
	if unit := daemon.durable(1); !slices.Equal(sends, []string{first.RawTxHex, second.RawTxHex, first.RawTxHex}) || !reverted || unit.Status != "uncertain" || unit.RawTxHex != first.RawTxHex {
		t.Fatalf("covered bytes were not sent to close their nonce: reverted=%t %+v", reverted, unit)
	}
	daemon.step(false)
	daemon.advance(time.Minute)
	daemon.step(false)
	daemon.step(false)
	if unit, other := daemon.durable(1), daemon.durable(2); unit.Status != "idle" || unit.Claimed != claimV2TestDec(5) || unit.Attempts != 0 || other.Status != "idle" || other.Claimed != claimV2TestDec(6) {
		t.Fatalf("units after the gap closed: %+v %+v", unit, other)
	}
	chain.finalize(claimV2TestAccount(claimV2TestOther, 2), claimV2TestAccount(claimV2TestColdkey, 8), claimV2TestAccount(claimV2TestSecond, 6))
	daemon.step(false)
	sends = chain.sent()
	claimed := chain.claimedOf(1)
	if len(sends) != 5 || sends[3] != second.RawTxHex || claimV2TestNonce(t, sends[4]) != 2 || claimed.Dec() != claimV2TestDec(8) {
		t.Fatalf("later claims did not proceed: sends=%d claimed=%s", len(sends), claimed.Dec())
	}
}

// E-5 an on-demand claim: below the threshold nothing is claimed until a
// demand, which then pays everything claimable and is spent.
func TestClaimV2OnDemandClaim(t *testing.T) {
	chain := newClaimV2TestChain(t)
	chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 1))
	daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
	daemon.step(false)
	if len(chain.sent()) != 0 {
		t.Fatal("below-threshold account was claimed without a demand")
	}
	daemon.step(true)
	if unit := daemon.durable(1); len(chain.sent()) != 1 || unit.Status != "idle" || unit.Claimed != claimV2TestDec(1) || unit.Demand {
		t.Fatalf("demanded claim: %+v", unit)
	}
	daemon.step(true)
	chain.finalize(claimV2TestAccount(claimV2TestOther, 2), claimV2TestAccount(claimV2TestColdkey, 1.5))
	daemon.step(false)
	if unit := daemon.durable(1); len(chain.sent()) != 1 || unit.Status != "idle" || unit.Demand {
		t.Fatalf("a spent demand claimed again: %+v", unit)
	}
}

// E-5, §10.1 refusal: a proof that does not reproduce the finalized on-chain root
// is refused before signing, and the unit retries.
func TestClaimV2RefusesUnverifiedProof(t *testing.T) {
	for name, tamper := range map[string]func(map[string]any){
		"sibling hash": func(entry map[string]any) {
			entry["proof"].([]map[string]any)[0]["hash"] = "0x" + strings.Repeat("ab", 32)
		},
		"cumulative": func(entry map[string]any) { entry["cumulative"] = claimV2TestDec(6) },
		"root index": func(entry map[string]any) { entry["root_index"] = 5 },
	} {
		t.Run(name, func(t *testing.T) {
			chain := newClaimV2TestChain(t)
			chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
			chain.with(func() { chain.account = tamper })
			daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
			daemon.step(false)
			unit := daemon.durable(1)
			if len(chain.sent()) != 0 || unit.Status != "open" || !strings.Contains(unit.LastError, errClaimV2Proof.Error()) || unit.NextRetryAt != daemon.clock.Add(time.Minute).Format(time.RFC3339Nano) || unit.Attempts != 0 {
				t.Fatalf("unverified proof: %+v", unit)
			}
		})
	}
}

// E-5 and §7.3: a claim that reverts after inclusion, or at preflight (the
// transfer floor, a staking-hotkey cap), stays open under the submission
// backoff and is claimed once the refusal clears. It is never marked done.
func TestClaimV2RevertedClaimStaysOpen(t *testing.T) {
	t.Run("after inclusion", func(t *testing.T) {
		chain := newClaimV2TestChain(t)
		chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
		daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
		chain.with(func() { chain.revertMined = true })
		daemon.step(false)
		daemon.advance(time.Minute)
		daemon.step(false)
		unit := daemon.durable(1)
		if unit.Status != "open" || unit.Attempts != 1 || unit.TxHash != "" || unit.RawTxHex != "" || !strings.Contains(unit.LastError, "reverted in finalized block") || unit.NextRetryAt != daemon.clock.Add(claimRetry(1)).Format(time.RFC3339Nano) {
			t.Fatalf("reverted claim: %+v", unit)
		}
		chain.with(func() { chain.revertMined = false })
		daemon.step(false)
		if len(chain.sent()) != 1 {
			t.Fatal("reverted claim retried before its backoff")
		}
		daemon.advance(claimRetry(1))
		daemon.step(false)
		sends, claimed := chain.sent(), chain.claimedOf(1)
		if unit := daemon.durable(1); len(sends) != 2 || sends[0] == sends[1] || claimed.Dec() != claimV2TestDec(5) || unit.Status != "idle" || unit.Attempts != 0 {
			t.Fatalf("retry after a revert: sends=%d %+v", len(sends), unit)
		}
	})
	t.Run("at preflight", func(t *testing.T) {
		chain := newClaimV2TestChain(t)
		chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
		daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
		chain.with(func() { chain.refuseClaims = true })
		for attempt := 1; attempt <= 3; attempt++ {
			daemon.step(false)
			unit := daemon.durable(1)
			if unit.Status != "open" || unit.Attempts != attempt || !strings.Contains(unit.LastError, "preflight") || unit.NextRetryAt != daemon.clock.Add(claimRetry(attempt)).Format(time.RFC3339Nano) {
				t.Fatalf("refused claim attempt %d: %+v", attempt, unit)
			}
			daemon.advance(claimRetry(attempt))
		}
		chain.with(func() { chain.refuseClaims = false })
		daemon.step(false)
		if unit := daemon.durable(1); len(chain.sent()) != 1 || unit.Status != "idle" || unit.Claimed != claimV2TestDec(5) {
			t.Fatalf("claim after the refusal cleared: %+v", unit)
		}
	})
}
