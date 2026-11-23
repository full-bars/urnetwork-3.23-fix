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
