package connect

import (
	"testing"
)

// Benchmarks backing the Task 17 data-plane audit: an unguarded
// V(n).Infof(...) call constructs and heap-boxes its arguments even when the
// level is disabled (provider ships with -v=0, so this is 100% waste in
// production). The guarded form must report 0 B/op and 0 allocs/op.
//
// Run: go test -run '^$' -bench 'BenchmarkVerbose|BenchmarkClientRunLog|BenchmarkPacketLog' -benchtime 500000x .

var (
	benchId   = Id{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	benchInt  = 42
	benchTag  = "client-abc"
	benchSink any
)

// BenchmarkClientRunLogUnguarded mirrors transfer.go:1048 (Client.run,
// per-inbound-message dispatch): 4 args, 3 of them Id.
func BenchmarkClientRunLogUnguarded(b *testing.B) {
	logger := NewGlogLogger()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		logger.V(1).Infof("[cr] %s %s<-%s s(%s)\n", benchTag, benchId, benchId, benchId)
	}
}

// BenchmarkClientRunLogGuarded is the same site with an .Enabled() guard.
func BenchmarkClientRunLogGuarded(b *testing.B) {
	logger := NewGlogLogger()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if v := logger.V(1); v.Enabled() {
			v.Infof("[cr] %s %s<-%s s(%s)\n", benchTag, benchId, benchId, benchId)
		}
	}
}

// BenchmarkPacketLogUnguarded mirrors the ip.go per-packet shape: 2 args.
func BenchmarkPacketLogUnguarded(b *testing.B) {
	logger := NewGlogLogger()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		logger.V(1).Infof("[f%d]udp receive %d\n", benchInt, benchInt)
	}
}

// BenchmarkPacketLogGuarded is the same site with an .Enabled() guard.
func BenchmarkPacketLogGuarded(b *testing.B) {
	logger := NewGlogLogger()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if v := logger.V(1); v.Enabled() {
			v.Infof("[f%d]udp receive %d\n", benchInt, benchInt)
		}
	}
}
