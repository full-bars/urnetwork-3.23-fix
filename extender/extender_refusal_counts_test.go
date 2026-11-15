package extender

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestRefusalCountsBounded(t *testing.T) {
	var server ExtenderServer
	for _, stage := range extenderRefusalStageNames {
		server.usage.recordRefusal(stage)
	}
	server.usage.recordRefusal("future stage")
	counts := server.RefusalCounts()
	for _, stage := range extenderRefusalStageNames {
		want := uint64(1)
		if stage == "other" {
			want = 2
		}
		if counts[stage] != want {
			t.Errorf("%s = %d, want %d", stage, counts[stage], want)
		}
	}
	if got := testing.AllocsPerRun(1000, func() { server.usage.recordRefusal("service") }); got != 0 {
		t.Fatalf("recordRefusal allocated %v times", got)
	}
}

func TestCarrierIdentityAttachment(t *testing.T) {
	if got := extenderCarrierPortLabel(connect.ExtenderConnectModeTcpTls, 443); got != ExtenderCarrierTcp443 {
		t.Fatalf("tcp 443 label = %q, want %q", got, ExtenderCarrierTcp443)
	}
	if got := extenderCarrierPortLabel(connect.ExtenderConnectModeQuic, 443); got != ExtenderCarrierUdp443 {
		t.Fatalf("quic 443 label = %q, want %q", got, ExtenderCarrierUdp443)
	}
	if got := extenderCarrierPortLabel(connect.ExtenderConnectModeDns, 4053); got != ExtenderCarrierUdp4053 {
		t.Fatalf("dns 4053 label = %q, want %q", got, ExtenderCarrierUdp4053)
	}
	if got := extenderCarrierPortLabel(connect.ExtenderConnectModeDns, connect.DefaultDnsPort); got != ExtenderCarrierUdp53 {
		t.Fatalf("dns 53 label = %q, want %q", got, ExtenderCarrierUdp53)
	}

	for _, carrier := range ExtenderCarrierPorts() {
		ctx := newExtenderRequestContextWithCarrier(context.Background(), "sni.example", carrier)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://sni.example/", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := extenderRequestCarrier(req); got != carrier {
			t.Fatalf("request carrier = %q, want %q", got, carrier)
		}
	}
}

func TestCarrierServedAndRefusalCounts(t *testing.T) {
	var server ExtenderServer
	for i, carrier := range ExtenderCarrierPorts() {
		for j := 0; j <= i; j++ {
			server.usage.recordCarrierServed(carrier)
		}
		server.usage.recordCarrierRefusal(carrier, "service")
		server.usage.recordCarrierRefusal(carrier, "destination authorization")
	}
	server.usage.recordCarrierRefusal(ExtenderCarrierTcp443, "unknown stage")

	served := server.CarrierServedCounts()
	refused := server.CarrierRefusalCounts()
	stats := server.CarrierStats()

	for i, carrier := range ExtenderCarrierPorts() {
		wantServed := uint64(i + 1)
		if served[carrier] != wantServed {
			t.Fatalf("%s served = %d, want %d", carrier, served[carrier], wantServed)
		}
		if refused[carrier]["service"] != 1 || refused[carrier]["destination authorization"] != 1 {
			t.Fatalf("%s refusals = %v", carrier, refused[carrier])
		}
		wantRefused := uint64(2)
		if carrier == ExtenderCarrierTcp443 {
			wantRefused = 3
			if refused[carrier]["other"] != 1 {
				t.Fatalf("tcp-443 other refusal = %d, want 1", refused[carrier]["other"])
			}
		}
		if stats[carrier].Served != wantServed || stats[carrier].Refused != wantRefused {
			t.Fatalf("%s stats = %+v, want served=%d refused=%d", carrier, stats[carrier], wantServed, wantRefused)
		}
	}

	if got := testing.AllocsPerRun(1000, func() {
		server.usage.recordCarrierServed(ExtenderCarrierUdp4053)
		server.usage.recordCarrierRefusal(ExtenderCarrierUdp53, "service")
	}); got != 0 {
		t.Fatalf("carrier counter hot path allocated %v times", got)
	}
}

func TestCarrierRefusalAndIntervalSummaryLogging(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var server ExtenderServer
	server.settings = &ExtenderSettings{
		AdmissionNow: func() time.Time { return now },
	}

	var logBuf bytes.Buffer
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	})

	if line := server.FlushCarrierSummaryLog(now); line != "" {
		t.Fatalf("quiet initial minute produced %q, want empty", line)
	}

	server.usage.recordCarrierServed(ExtenderCarrierTcp443)
	server.usage.recordCarrierServed(ExtenderCarrierTcp443)
	server.usage.recordCarrierServed(ExtenderCarrierUdp443)
	server.usage.recordCarrierServed(ExtenderCarrierUdp4053)
	server.usage.recordCarrierServed(ExtenderCarrierUdp53)
	server.usage.recordCarrierRefusal(ExtenderCarrierTcp443, "service")

	server.noteCarrierRefusal(ExtenderCarrierTcp443, "service", errors.New("service 1 is not available"))
	server.noteCarrierRefusal(ExtenderCarrierTcp443, "service", errors.New("service 1 is not available"))
	if !strings.Contains(logBuf.String(), "refused a request at service on tcp-443: service 1 is not available") {
		t.Fatalf("first refusal log missing carrier:\n%s", logBuf.String())
	}

	now = now.Add(time.Minute)
	logBuf.Reset()
	server.usage.recordCarrierRefusal(ExtenderCarrierTcp443, "service")
	server.noteCarrierRefusal(ExtenderCarrierTcp443, "service", errors.New("service 1 is not available"))

	out := logBuf.String()
	for _, want := range []string{
		"last minute: tcp-443 served 2 refused 2 | udp-443 served 1 refused 0 | udp-4053 served 1 refused 0 | udp-53 served 1 refused 0",
		"refused 2 requests at service on tcp-443 in the last minute; latest: service 1 is not available",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("interval log missing %q:\n%s", want, out)
		}
	}

	now = now.Add(time.Minute)
	if line := server.FlushCarrierSummaryLog(now); line != "" {
		t.Fatalf("quiet minute after flush produced %q, want empty", line)
	}
}
