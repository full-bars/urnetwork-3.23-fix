package main

import (
	"strings"
	"testing"
)

// The reconcile is fully unit-tested but a function nothing calls fixes
// nothing, which is how the last feature shipped inert. So prove by reading the
// source that the takeover path in provide() launches it, with the provider's
// ctx and a wait that outlasts the parent's drain, and after the takeover merge.
func TestProvideReconcilesTheAuditRingAfterTakeover(t *testing.T) {
	body := provideFuncBody(t)

	merge := body.firstIndex("mergeAuditRingFromDisk")
	if merge < 0 {
		t.Fatal("provide() no longer calls mergeAuditRingFromDisk at takeover")
	}
	rec := body.firstIndex("reconcileAuditRingAfterHandoff")
	if rec < 0 {
		t.Fatal("provide() never calls reconcileAuditRingAfterHandoff: the parent's " +
			"drain-end persist can leave a parent-only ring on disk and a crash soon " +
			"after takeover loses the successor's start entry")
	}
	if rec < merge {
		t.Errorf("the reconcile (line %d) is scheduled before the takeover merge (line %d)", rec, merge)
	}

	call := body.callWithFirstArg("reconcileAuditRingAfterHandoff")
	if call == nil || len(call.args) != 2 {
		t.Fatal("reconcileAuditRingAfterHandoff is not called with (ctx, wait)")
	}
	if got := call.args[0].text; got != "ctx" {
		t.Errorf("reconcile is passed %q as its context, want the provider's ctx: a "+
			"fresh context outlives shutdown and writes the file after main()'s final persist", got)
	}
	wait := strings.ReplaceAll(call.args[1].text, " ", "")
	if wait != "HotSwapDrainTimeout+auditReconcileGrace" {
		t.Errorf("reconcile waits %q, want HotSwapDrainTimeout+auditReconcileGrace: "+
			"any shorter and it can run before the parent's drain-end persist and be "+
			"overwritten by it", wait)
	}
}
