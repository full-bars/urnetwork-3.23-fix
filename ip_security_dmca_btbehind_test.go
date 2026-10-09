package connect

import (
	"net/netip"
	"testing"
)

// A packet of n bytes of high-entropy filler with the BitTorrent handshake
// string placed at offset, the way a peer would hide it behind a forged
// application header.
func payloadWithBittorrentAt(n int, offset int) []byte {
	b := encryptedPayload(n)
	copy(b[offset:], bittorrentHandshakePrefix)
	return b
}

func newBehindHeaderDetector() *dmcaDetector {
	return newDmcaDetector(DefaultDmcaSecurityPolicySettings(), newWebStandardDetector(DefaultWebStandardSettings()))
}

func fixturePayloadsForTest(t *testing.T, name string) [][]byte {
	t.Helper()
	fixture := loadSecurityFixture(t, "testdata/ipsecurity/"+name+".json")
	return fixture.payloadBytes(t)
}

// The invariant the PR states: no BitTorrent payload rides behind an
// application standard's header, on any admit path.
func TestAdmitPathsRejectBittorrentBehindHeader(t *testing.T) {
	steam := steamValveNetworkPrefixes[0].Masked().Addr()
	meta := metaNetworkPrefixes[0].Masked().Addr()

	t.Run("gaming exception, marker at 16", func(t *testing.T) {
		d := newBehindHeaderDetector()
		path := steamTestPath(steam, IpProtocolUdp, 27015, false)
		if v := d.classify(path, payloadWithBittorrentAt(64, 16)); v != dmcaBittorrent {
			t.Fatalf("verdict = %v, want bittorrent", v)
		}
	})

	t.Run("gaming exception, marker on a later packet", func(t *testing.T) {
		d := newBehindHeaderDetector()
		path := steamTestPath(steam, IpProtocolUdp, 27015, false)
		if v := d.classify(path, encryptedPayload(64)); v == dmcaBittorrent {
			t.Fatal("a plain first packet must not be bittorrent")
		}
		if v := d.classify(path, payloadWithBittorrentAt(64, 16)); v != dmcaBittorrent {
			t.Fatalf("later packet verdict = %v, want bittorrent", v)
		}
	})

	t.Run("messaging backstop, marker at 16", func(t *testing.T) {
		d := newBehindHeaderDetector()
		path := whatsAppTestPath(meta.String(), IpProtocolTcp, 5222, false)
		if v := d.classify(path, payloadWithBittorrentAt(64, 16)); v != dmcaBittorrent {
			t.Fatalf("verdict = %v, want bittorrent", v)
		}
	})

	t.Run("wireguard two-packet confirm, marker behind the transport header", func(t *testing.T) {
		d := newBehindHeaderDetector()
		path := dmcaPath(IpProtocolUdp, 47001, 51820, false)
		wg := fixturePayloadsForTest(t, "wireguard-handshake")
		if v := d.classify(path, wg[0]); v == dmcaBittorrent {
			t.Fatal("the initiation must not be bittorrent")
		}
		confirm := payloadWithBittorrentAt(64, 16)
		copy(confirm, []byte{4, 0, 0, 0}) // type 4 transport, reserved zeros
		if v := d.classify(path, confirm); v != dmcaBittorrent {
			t.Fatalf("confirm verdict = %v, want bittorrent", v)
		}
	})

	t.Run("rtmp single packet, marker one byte past the header", func(t *testing.T) {
		d := newBehindHeaderDetector()
		path := dmcaPath(IpProtocolTcp, 47002, 1935, true)
		first := make([]byte, 64)
		first[0] = 3 // C0 version, C1 time and zero word
		copy(first[10:], bittorrentHandshakePrefix)
		if v := d.classify(path, first); v != dmcaBittorrent {
			t.Fatalf("verdict = %v, want bittorrent", v)
		}
	})

	t.Run("app admitted flow, marker on a later packet", func(t *testing.T) {
		d := newBehindHeaderDetector()
		path := dmcaPath(IpProtocolTcp, 47003, 1935, true)
		rtmp := fixturePayloadsForTest(t, "rtmp-publish")
		if v := d.classify(path, rtmp[0]); v == dmcaBittorrent || v == dmcaDropEncrypted {
			t.Fatalf("legitimate rtmp opener verdict = %v", v)
		}
		if v := d.classify(path, payloadWithBittorrentAt(128, 40)); v != dmcaBittorrent {
			t.Fatalf("later packet verdict = %v, want bittorrent", v)
		}
	})
}

func TestAnyOffsetBittorrentMarker(t *testing.T) {
	if anyOffsetBittorrentMarker(encryptedPayload(512)) {
		t.Fatal("uniform filler must not look like bittorrent")
	}
	for _, offset := range []int{0, 1, 13, 200} {
		if !anyOffsetBittorrentMarker(payloadWithBittorrentAt(256, offset)) {
			t.Fatalf("handshake at offset %d not found", offset)
		}
	}
	if !anyOffsetBittorrentMarker([]byte("xxGET /announce?info_hash=abc HTTP/1.1\r\n")) {
		t.Fatal("tracker query not found")
	}
	_ = netip.Addr{}
}
