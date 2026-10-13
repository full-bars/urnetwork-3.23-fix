package connect

import (
	"testing"
	"time"
)

// the provider return path calls touchFlow for every relayed packet
func BenchmarkDmcaTouchFlow(b *testing.B) {
	detector := newIdleDmcaDetector(nil, 10*time.Minute)
	path := dmcaIdleTestPath(6881)
	detector.classify(path, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		detector.touchFlow(path)
	}
}

func BenchmarkDmcaTouchFlowParallel(b *testing.B) {
	detector := newIdleDmcaDetector(nil, 10*time.Minute)
	path := dmcaIdleTestPath(6881)
	detector.classify(path, nil)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			detector.touchFlow(path)
		}
	})
}
