package connect

import "testing"

// BenchmarkBillablePacketAccounting isolates the atomic updates used for each IP packet.
func BenchmarkBillablePacketAccounting(b *testing.B) {
	for _, direction := range []string{"rx", "tx"} {
		b.Run(direction, func(b *testing.B) {
			var bw ProxyBandwidth
			packet := make([]byte, 1500)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if direction == "rx" {
					bw.BillableRx.Add(uint64(len(packet)))
					bw.BillableRxPackets.Add(1)
				} else {
					bw.BillableTx.Add(uint64(len(packet)))
					bw.BillableTxPackets.Add(1)
				}
			}
		})
	}
}

// BenchmarkBillableFamilyAccounting measures the accounting call the receive and
// send paths make now, next to the two atomic adds they made before the IP
// family counters. An IPv4 packet must match the base cost: the family is read
// from the header and nothing else is written. An IPv6 packet pays the two
// extra adds that keep its share.
func BenchmarkBillableFamilyAccounting(b *testing.B) {
	packet4 := make([]byte, 1500)
	packet4[0] = 0x45
	packet6 := make([]byte, 1500)
	packet6[0] = 0x60
	path4 := &IpPath{Version: 4}
	path6 := &IpPath{Version: 6}

	b.Run("base only", func(b *testing.B) {
		var bw ProxyBandwidth
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			bw.BillableRx.Add(uint64(len(packet4)))
			bw.BillableRxPackets.Add(1)
			bw.BillableTx.Add(uint64(len(packet4)))
			bw.BillableTxPackets.Add(1)
		}
	})

	b.Run("ipv4", func(b *testing.B) {
		p := &RemoteUserNatProvider{bw: &ProxyBandwidth{}}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			p.countBillableRx(path4, packet4)
			p.countBillableTx(packet4)
		}
	})

	b.Run("ipv6", func(b *testing.B) {
		p := &RemoteUserNatProvider{bw: &ProxyBandwidth{}}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			p.countBillableRx(path6, packet6)
			p.countBillableTx(packet6)
		}
	})
}
