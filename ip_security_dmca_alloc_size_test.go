package connect

import (
	"context"
	"net"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

func TestDmcaFlowStateStructSize(t *testing.T) {
	size := unsafe.Sizeof(dmcaFlowState{})
	t.Logf("unsafe.Sizeof(dmcaFlowState{}): %d bytes", size)
	// 184 bytes today (104 before the application-standard state). Every
	// tracked flow carries one, up to MaxFlows, so growth must be deliberate.
	const maxFlowStateBytes = 192
	if maxFlowStateBytes < size {
		t.Fatalf("dmcaFlowState is %d bytes, ceiling %d", size, maxFlowStateBytes)
	}
}

func TestDmcaPerFlowMemoryBytes(t *testing.T) {
	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)

	const flowCount = 10000
	settings := DefaultDmcaSecurityPolicySettings()
	settings.MaxFlows = flowCount * 2
	webSettings := DefaultWebStandardSettings()
	detector := newDmcaDetector(settings, newWebStandardDetector(webSettings))

	payload := []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	for i := 0; i < flowCount; i++ {
		srcPort := 1024 + (i % 60000)
		dstPort := 10000 + (i / 60000)
		path := &IpPath{
			Version:         4,
			Protocol:        IpProtocolTcp,
			SourceIp:        net.IPv4(10, byte(i>>16), byte(i>>8), byte(i)),
			SourcePort:      srcPort,
			DestinationIp:   net.IPv4(198, 51, 100, 1),
			DestinationPort: dstPort,
			Syn:             true,
		}
		detector.classify(path, payload)
	}

	runtime.GC()
	var m2 runtime.MemStats
	runtime.ReadMemStats(&m2)

	allocatedBytes := int64(m2.HeapAlloc) - int64(m1.HeapAlloc)
	perFlowBytes := allocatedBytes / int64(flowCount)
	t.Logf("Total tracked flows: %d", detector.Testing_FlowCount())
	t.Logf("HeapAlloc delta: %d bytes across %d flows -> ~%d bytes/flow", allocatedBytes, flowCount, perFlowBytes)
	if detector.Testing_FlowCount() != flowCount {
		t.Fatalf("expected %d tracked flows, got %d", flowCount, detector.Testing_FlowCount())
	}
	// ~307 bytes/flow measured (state plus map entry); the ceiling leaves room
	// for GC noise but catches a doubling.
	const maxPerFlowBytes = 600
	if maxPerFlowBytes < perFlowBytes {
		t.Fatalf("per-flow heap %d bytes, ceiling %d", perFlowBytes, maxPerFlowBytes)
	}
	runtime.KeepAlive(detector)
}

func BenchmarkDmcaPerFlowBytes(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		st := &dmcaFlowState{
			key: Ip6Path{
				SourcePort:      int(i),
				DestinationPort: 8080,
			},
		}
		if st.terminal != 0 {
			b.Fatal("unexpected terminal")
		}
	}
}

func TestDmcaAppStandardZeroAllocs(t *testing.T) {
	settings := DefaultDmcaSecurityPolicySettings()
	webSettings := DefaultWebStandardSettings()
	detector := newDmcaDetector(settings, newWebStandardDetector(webSettings))

	t.Run("decided flow", func(t *testing.T) {
		path := &IpPath{
			Version:         4,
			Protocol:        IpProtocolTcp,
			SourceIp:        net.IPv4(10, 0, 0, 1).To16(),
			SourcePort:      41001,
			DestinationIp:   net.IPv4(198, 51, 100, 1).To16(),
			DestinationPort: 8080,
			Syn:             true,
		}
		payload := []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
		// Warm up and reach terminal allow
		detector.classify(path, payload)

		allocs := testing.AllocsPerRun(100, func() {
			if v := detector.classify(path, payload); v != dmcaAllow {
				t.Fatalf("unexpected verdict: %v", v)
			}
		})
		if allocs != 0 {
			t.Fatalf("decided flow allocs = %v, want 0", allocs)
		}
	})

	t.Run("WireGuard path", func(t *testing.T) {
		path := &IpPath{
			Version:         4,
			Protocol:        IpProtocolUdp,
			SourceIp:        net.IPv4(10, 0, 0, 2).To16(),
			SourcePort:      41002,
			DestinationIp:   net.IPv4(198, 51, 100, 2).To16(),
			DestinationPort: 51820,
		}
		fixture := loadSecurityFixture(t, filepath.Join("testdata", "ipsecurity", "wireguard-handshake.json"))
		packets := fixturePackets(t, fixture, 41002)
		_, payload, err := ParseIpPathWithPayload(packets[0])
		if err != nil {
			t.Fatal(err)
		}
		// Warm up flow state through confirmation packet
		detector.classify(path, payload)
		_, payload1, err := ParseIpPathWithPayload(packets[1])
		if err != nil {
			t.Fatal(err)
		}
		detector.classify(path, payload1)

		allocs := testing.AllocsPerRun(100, func() {
			v := detector.classify(path, payload1)
			if v != dmcaAllow {
				t.Fatalf("unexpected verdict: %v", v)
			}
		})
		if allocs != 0 {
			t.Fatalf("WireGuard path allocs = %v, want 0", allocs)
		}
	})

	t.Run("OpenVPN path", func(t *testing.T) {
		path := &IpPath{
			Version:         4,
			Protocol:        IpProtocolUdp,
			SourceIp:        net.IPv4(10, 0, 0, 3).To16(),
			SourcePort:      41003,
			DestinationIp:   net.IPv4(198, 51, 100, 3).To16(),
			DestinationPort: 1194,
		}
		fixture := loadSecurityFixture(t, filepath.Join("testdata", "ipsecurity", "openvpn-udp.json"))
		packets := fixturePackets(t, fixture, 41003)
		_, payload, err := ParseIpPathWithPayload(packets[0])
		if err != nil {
			t.Fatal(err)
		}
		// Warm up flow state
		detector.classify(path, payload)

		allocs := testing.AllocsPerRun(100, func() {
			v := detector.classify(path, payload)
			if v != dmcaAllow {
				t.Fatalf("unexpected verdict: %v", v)
			}
		})
		if allocs != 0 {
			t.Fatalf("OpenVPN path allocs = %v, want 0", allocs)
		}
	})

	t.Run("WhatsApp path", func(t *testing.T) {
		path := &IpPath{
			Version:         4,
			Protocol:        IpProtocolTcp,
			SourceIp:        net.IPv4(10, 0, 0, 4).To16(),
			SourcePort:      41004,
			DestinationIp:   net.IPv4(198, 51, 100, 4).To16(),
			DestinationPort: 5222,
			Syn:             true,
		}
		fixture := loadSecurityFixture(t, filepath.Join("testdata", "ipsecurity", "whatsapp-noise-web.json"))
		packets := fixturePackets(t, fixture, 41004)
		_, payload, err := ParseIpPathWithPayload(packets[0])
		if err != nil {
			t.Fatal(err)
		}
		// Warm up flow state through confirmation packet
		detector.classify(path, nil)
		data := *path
		data.Syn = false
		detector.classify(&data, payload)
		_, payload1, err := ParseIpPathWithPayload(packets[1])
		if err != nil {
			t.Fatal(err)
		}
		detector.classify(&data, payload1)

		allocs := testing.AllocsPerRun(100, func() {
			v := detector.classify(&data, payload1)
			if v != dmcaAllow {
				t.Fatalf("unexpected verdict: %v", v)
			}
		})
		if allocs != 0 {
			t.Fatalf("WhatsApp path allocs = %v, want 0", allocs)
		}
	})

	t.Run("Ethereum path", func(t *testing.T) {
		path := &IpPath{
			Version:         4,
			Protocol:        IpProtocolUdp,
			SourceIp:        net.IPv4(10, 0, 0, 5).To16(),
			SourcePort:      41005,
			DestinationIp:   net.IPv4(198, 51, 100, 5).To16(),
			DestinationPort: 30303,
		}
		fixture := loadSecurityFixture(t, filepath.Join("testdata", "ipsecurity", "ethereum-discv4.json"))
		packets := fixturePackets(t, fixture, 41005)
		_, payload, err := ParseIpPathWithPayload(packets[0])
		if err != nil {
			t.Fatal(err)
		}
		// Warm up flow state
		detector.classify(path, payload)

		allocs := testing.AllocsPerRun(100, func() {
			v := detector.classify(path, payload)
			if v != dmcaAllow {
				t.Fatalf("unexpected verdict: %v", v)
			}
		})
		if allocs != 0 {
			t.Fatalf("Ethereum path allocs = %v, want 0", allocs)
		}
	})

	t.Run("Meta path", func(t *testing.T) {
		metaAddr := metaTestAddress(t, 4, 0)
		path := whatsAppTestPath(metaAddr, IpProtocolTcp, 5222, true)
		payload := encryptedPayload(512)
		// Warm up flow state
		detector.classify(path, nil)
		data := *path
		data.Syn = false
		detector.classify(&data, payload)

		allocs := testing.AllocsPerRun(100, func() {
			v := detector.classify(&data, payload)
			if v != dmcaAllow {
				t.Fatalf("unexpected verdict: %v", v)
			}
		})
		if allocs != 0 {
			t.Fatalf("Meta path allocs = %v, want 0", allocs)
		}
	})
}

func TestDpiDefaultsRestorePrePortVerdicts(t *testing.T) {
	// Pre-port policy settings: no App, Gaming, or Messaging; WebStandard Turn/Rtp/Rtcp disabled;
	// CFAA AllowTelegramCalls disabled; InspectPrivilegedSignatures disabled.
	// The production default: neither variable set.
	t.Setenv("URNETWORK_DPI_ADMITS", "")
	t.Setenv("URNETWORK_DPI_PRIVILEGED_BT", "")
	resetDpiAdmitsEnvForTest()
	t.Cleanup(resetDpiAdmitsEnvForTest)
	if DpiAdmitsEnabled() || DpiPrivilegedBtEnabled() {
		t.Fatalf("DPI admits and privileged BT must default off, got admits=%t privileged_bt=%t", DpiAdmitsEnabled(), DpiPrivilegedBtEnabled())
	}

	ctx := context.Background()
	policy := DefaultSecurityPolicy(ctx)

	// 1. Privileged port 443 BitTorrent handshake: pre-port was allowed without inspection;
	// with URNETWORK_DPI_PRIVILEGED_BT unset, it remains allowed (restores pre-change behavior).
	btPath := dmcaPath(IpProtocolTcp, 43001, 443, false)
	r, err := policy.InspectEgress(protocol.ProvideMode_Public, btPath, btHandshake())
	if err != nil || r != SecurityPolicyResultAllow {
		t.Fatalf("privileged port 443 with PRIVILEGED_BT=0: got (%v, %v), want (Allow, nil)", r, err)
	}

	// 2. WireGuard, OpenVPN, WhatsApp, Ethereum, and Gaming:
	// With ADMITS=off, they look like unknown encrypted traffic to high ports,
	// so the entropy heuristic drops them after decision packets (pre-port behavior).
	fixturesToDrop := []string{
		"wireguard-handshake",
		"openvpn-udp",
		"openvpn-tcp",
		"ethereum-discv4",
		"ethereum-rlpx-eip8",
	}
	for i, name := range fixturesToDrop {
		fixture := loadSecurityFixture(t, filepath.Join("testdata", "ipsecurity", name+".json"))
		results := replayFixture(t, policy, fixture, 43100+i)
		last := results[len(results)-1]
		if last != SecurityPolicyResultDrop {
			t.Fatalf("fixture %s with ADMITS=off got %v, want Drop", name, last)
		}
	}

	// 3. Telegram call reflector on port 596:
	// With ADMITS=off, AllowTelegramCalls is false, so privileged port 596 is dropped by CFAA.
	telegramPath := &IpPath{
		Version:         4,
		Protocol:        IpProtocolUdp,
		SourceIp:        securityFixtureSourceIp,
		DestinationIp:   net.ParseIP("91.108.13.2").To4(),
		DestinationPort: 596,
	}
	r, err = policy.InspectEgress(protocol.ProvideMode_Public, telegramPath, encryptedPayload(512))
	if err != nil || r != SecurityPolicyResultDrop {
		t.Fatalf("Telegram call reflector with ADMITS=off: got (%v, %v), want (Drop, nil)", r, err)
	}

	// 4. Baseline web standards (TLS/QUIC) and standard BitTorrent still work normally.
	tlsFixture := loadSecurityFixture(t, filepath.Join("testdata", "ipsecurity", "tls-443.json"))
	tlsResults := replayFixture(t, policy, tlsFixture, 43200)
	if tlsResults[len(tlsResults)-1] != SecurityPolicyResultAllow {
		t.Fatalf("tls-443 with ADMITS=off got %v, want Allow", tlsResults[len(tlsResults)-1])
	}

	btHighFixture := loadSecurityFixture(t, filepath.Join("testdata", "ipsecurity", "bittorrent-tcp-51413.json"))
	btResults := replayFixture(t, policy, btHighFixture, 43300)
	if btResults[len(btResults)-1] != SecurityPolicyResultIncident {
		t.Fatalf("bittorrent-tcp-51413 with ADMITS=off got %v, want Incident", btResults[len(btResults)-1])
	}
}

// The privileged-port signature check runs on every packet to a port below
// 1024 when URNETWORK_DPI_PRIVILEGED_BT=1, which is most TLS and HTTP traffic.
func BenchmarkDmcaPrivilegedPortSignatureCheck(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		name := "off"
		if enabled {
			name = "on"
		}
		b.Run(name, func(b *testing.B) {
			settings := DefaultDmcaSecurityPolicySettings()
			settings.InspectPrivilegedSignatures = enabled
			detector := newDmcaDetector(settings, newWebStandardDetector(DefaultWebStandardSettings()))
			path := &IpPath{
				Version:         4,
				Protocol:        IpProtocolTcp,
				SourceIp:        net.IPv4(10, 0, 0, 1).To16(),
				SourcePort:      41001,
				DestinationIp:   net.IPv4(198, 51, 100, 1).To16(),
				DestinationPort: 443,
			}
			payload := make([]byte, 1400)
			payload[0], payload[1], payload[2] = 0x17, 0x03, 0x03
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				detector.classify(path, payload)
			}
		})
	}
}
