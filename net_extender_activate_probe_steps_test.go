package connect

import (
	"encoding/json"
	"strings"
	"testing"
)

// What the client does with the operator's probe timings on a refusal
// (connect#230).
//
// The activation answer carries probe_steps: every step the operator's probe
// ran, with how long it ran, how much of the probe budget was left when it
// started, and how it ended. A refusal's bare error text reads the same whether
// the extender answered late or never answered at all, and the steps are the
// only thing that tells them apart. Before this, the whole answer after
// `error` was decoded into nothing, so the timing the operator sends was
// thrown away on arrival.
//
// The wire forms below are the operator's, copied from urnetwork/server
// api/handlers/extender_handlers_test.go, so a change on either side fails
// here rather than in the field.

const testActivationRefusalWithSteps = `{"activated":false,"probe_steps":[` +
	`{"name":"tcp","ip_version":4,"port":443,"elapsed_ms":9500,"budget_left_ms":10000,"outcome":"ok"},` +
	`{"name":"forward","ip_version":4,"port":443,"elapsed_ms":500,"budget_left_ms":500,"outcome":"timeout"}],` +
	`"error":"the forward did not answer within the 0.5 s left of the 10 s probe budget: context deadline exceeded"}`

const testActivationAcceptedWithRefusedCarriers = `{"activated":true,"carriers":["tcp","dns"],"dns_ports":[4053],"refused_carriers":[` +
	`{"carrier":"quic","port":443,"error":"connection refused","elapsed_ms":12,"budget_left_ms":6400},` +
	`{"carrier":"dns","port":53,"error":"no answer within 3 s: context deadline exceeded","elapsed_ms":3000,"budget_left_ms":9400}]}`

const testActivationRefusalWithoutSteps = `{"activated":false,"error":"the forward did not answer"}`

func decodeTestActivation(t *testing.T, body string) *ExtenderActivateResult {
	t.Helper()
	result := &ExtenderActivateResult{}
	if err := json.Unmarshal([]byte(body), result); err != nil {
		t.Fatalf("the activation answer did not decode: %v", err)
	}
	return result
}

// A refusal's steps decode with their timing, in the order the operator ran
// them, and the error text stays what it was.
func TestExtenderActivateResultDecodesProbeSteps(t *testing.T) {
	result := decodeTestActivation(t, testActivationRefusalWithSteps)

	if result.Activated {
		t.Fatal("a refusal decoded as activated")
	}
	if !strings.Contains(result.Error, "the forward did not answer") {
		t.Errorf("error = %q, expected the operator's refusal text", result.Error)
	}
	if len(result.ProbeSteps) != 2 {
		t.Fatalf("probe steps = %d, expected 2", len(result.ProbeSteps))
	}
	tcp := result.ProbeSteps[0]
	if tcp.Name != "tcp" || tcp.IpVersion != 4 || tcp.Port != 443 || tcp.Outcome != "ok" {
		t.Errorf("first step = %+v, expected the tcp challenge passing", tcp)
	}
	if tcp.ElapsedMs != 9500 || tcp.BudgetLeftMs != 10000 {
		t.Errorf("first step timing = %d/%d, expected 9500/10000", tcp.ElapsedMs, tcp.BudgetLeftMs)
	}
	forward := result.ProbeSteps[1]
	if forward.Name != "forward" || forward.Outcome != "timeout" {
		t.Errorf("last step = %+v, expected the forward timing out", forward)
	}
	if forward.ElapsedMs != 500 || forward.BudgetLeftMs != 500 {
		t.Errorf("last step timing = %d/%d, expected 500/500", forward.ElapsedMs, forward.BudgetLeftMs)
	}
	// the step that refused it is the last one, which is what the log names
	if result.ProbeSteps[len(result.ProbeSteps)-1] != forward {
		t.Error("the refusing step is not the last one decoded")
	}
}

// An accepted answer's refused carriers and dns ports decode with their timing,
// so a carrier left out of the published record is visible.
func TestExtenderActivateResultDecodesRefusedCarriers(t *testing.T) {
	result := decodeTestActivation(t, testActivationAcceptedWithRefusedCarriers)

	if !result.Activated {
		t.Fatal("an accepted answer decoded as refused")
	}
	if len(result.DnsPorts) != 1 || result.DnsPorts[0] != 4053 {
		t.Errorf("dns ports = %v, expected [4053]", result.DnsPorts)
	}
	if len(result.RefusedCarriers) != 2 {
		t.Fatalf("refused carriers = %d, expected 2", len(result.RefusedCarriers))
	}
	quic := result.RefusedCarriers[0]
	if quic.Carrier != "quic" || quic.Port != 443 || quic.Error != "connection refused" {
		t.Errorf("first refusal = %+v, expected quic on 443 refused", quic)
	}
	if quic.ElapsedMs != 12 || quic.BudgetLeftMs != 6400 {
		t.Errorf("first refusal timing = %d/%d, expected 12/6400", quic.ElapsedMs, quic.BudgetLeftMs)
	}
	dns := result.RefusedCarriers[1]
	if dns.Carrier != "dns" || dns.Port != 53 || dns.ElapsedMs != 3000 {
		t.Errorf("second refusal = %+v, expected dns on 53 after 3 s", dns)
	}
}

// An answer from an operator that predates the fields decodes as it always did:
// the error intact, and the timing fields empty rather than zero-value steps
// that a log would present as measured.
func TestExtenderActivateResultWithoutProbeSteps(t *testing.T) {
	result := decodeTestActivation(t, testActivationRefusalWithoutSteps)

	if result.Activated {
		t.Fatal("a refusal decoded as activated")
	}
	if result.Error != "the forward did not answer" {
		t.Errorf("error = %q, expected the operator's refusal text", result.Error)
	}
	if result.ProbeSteps != nil {
		t.Errorf("probe steps = %+v, expected none from an older operator", result.ProbeSteps)
	}
	if result.RefusedCarriers != nil {
		t.Errorf("refused carriers = %+v, expected none from an older operator", result.RefusedCarriers)
	}
	if result.DnsPorts != nil {
		t.Errorf("dns ports = %+v, expected none from an older operator", result.DnsPorts)
	}
	if result.RetryAfterSeconds != 0 {
		t.Errorf("retry after = %d, expected 0", result.RetryAfterSeconds)
	}
}

// A logger whose verbose levels are on, so the debug branch of the step log can
// be exercised without glog. The verbose sink writes to the same recorder, which
// is what proves the step list goes through V(1).Infof rather than Infof.
type testVerboseActivateLogger struct {
	testActivateLogger
}

func (self *testVerboseActivateLogger) V(level int32) Verbose {
	return testEnabledVerbose{log: &self.testActivateLogger}
}

type testEnabledVerbose struct {
	log *testActivateLogger
}

func (testEnabledVerbose) Enabled() bool { return true }

func (self testEnabledVerbose) Info(args ...any) { self.log.Info(args...) }

func (self testEnabledVerbose) Infof(format string, args ...any) { self.log.Infof(format, args...) }

func newTestActivatorForLog(log Logger) *ExtenderActivator {
	return &ExtenderActivator{log: log}
}

// An answer with no steps logs nothing new: the refusal line is all there is,
// exactly as an older operator produced before.
func TestLogProbeStepsSilentWithoutSteps(t *testing.T) {
	for _, probeSteps := range [][]*ExtenderProbeStep{nil, {}, {nil, nil}} {
		log := &testActivateLogger{}
		newTestActivatorForLog(log).logProbeSteps(4, probeSteps)
		if len(log.lines) != 0 {
			t.Errorf("steps %v logged %d lines, expected none: %v", probeSteps, len(log.lines), log.lines)
		}
	}
}

// A refusal's step timing is logged at info, naming the step that refused it and
// the budget left when that step started. The per-step list is debug only, so a
// fleet log carries one line per refusal rather than one per step.
func TestLogProbeStepsAtInfo(t *testing.T) {
	log := &testActivateLogger{}
	steps := []*ExtenderProbeStep{
		{Name: "tcp", IpVersion: 4, Port: 443, ElapsedMs: 9500, BudgetLeftMs: 10000, Outcome: "ok"},
		{Name: "forward", IpVersion: 4, Port: 443, ElapsedMs: 500, BudgetLeftMs: 500, Outcome: "timeout"},
	}
	newTestActivatorForLog(log).logProbeSteps(4, steps)

	if len(log.lines) != 1 {
		t.Fatalf("logged %d lines, expected 1: %v", len(log.lines), log.lines)
	}
	line := log.lines[0]
	for _, expected := range []string{
		"🌉 [extender]activate v4 probe stopped at forward on port 443",
		"timeout after 500ms",
		"500ms of the budget left",
	} {
		if !strings.Contains(line, expected) {
			t.Errorf("the probe line %q does not say %q", line, expected)
		}
	}
	if strings.Contains(line, "probe step tcp") {
		t.Errorf("the probe line %q carries the per-step list at info", line)
	}
}

// In the fallback mode the loop tracks no family, so the refusal carries v0. The
// summary names the family the operator actually probed instead, since that is
// the fact the line exists to report.
func TestLogProbeStepsFallbackNamesTheOperatorsFamily(t *testing.T) {
	log := &testActivateLogger{}
	steps := []*ExtenderProbeStep{
		{Name: "tcp", IpVersion: 6, Port: 443, ElapsedMs: 900, BudgetLeftMs: 10000, Outcome: "ok"},
		{Name: "forward", IpVersion: 6, Port: 443, ElapsedMs: 300, BudgetLeftMs: 900, Outcome: "timeout"},
	}
	newTestActivatorForLog(log).logProbeSteps(0, steps)

	if len(log.lines) != 1 {
		t.Fatalf("logged %d lines, expected 1: %v", len(log.lines), log.lines)
	}
	line := log.lines[0]
	if !strings.Contains(line, "activate v6 probe stopped") {
		t.Errorf("the fallback refusal line %q does not name the probed family", line)
	}
	if strings.Contains(line, "v0") {
		t.Errorf("the fallback refusal line %q reports v0", line)
	}
}

// At debug every step is logged with its own timing, which is what a report to
// the operator carries.
func TestLogProbeStepsAtDebug(t *testing.T) {
	log := &testVerboseActivateLogger{}
	steps := []*ExtenderProbeStep{
		{Name: "tcp", IpVersion: 6, Port: 443, ElapsedMs: 9500, BudgetLeftMs: 10000, Outcome: "ok"},
		{Name: "forward", IpVersion: 6, Port: 443, ElapsedMs: 500, BudgetLeftMs: 500, Outcome: "timeout"},
	}
	newTestActivatorForLog(log).logProbeSteps(6, steps)

	if len(log.lines) != 3 {
		t.Fatalf("logged %d lines, expected the summary plus both steps: %v", len(log.lines), log.lines)
	}
	for index, step := range steps {
		line := log.lines[index+1]
		for _, expected := range []string{
			"probe step " + step.Name,
			"ipv6",
			"outcome=" + step.Outcome,
		} {
			if !strings.Contains(line, expected) {
				t.Errorf("the step line %q does not say %q", line, expected)
			}
		}
	}
}
