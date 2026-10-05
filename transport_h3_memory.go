package connect

import (
	"sync"

	quic "github.com/quic-go/quic-go"
)

// Memory bounds for the H3 carrier. quic-go left to itself auto-tunes a stream
// receive window up to 6 MiB and a connection window up to 15 MiB, and accepts
// 100 incoming streams, none of which a carrier that uses one stream needs.
// These are upstream's process-default limits: the initial credit is fixed, the
// ceilings scale down with the provider's memory budget (SetMemoryBudget) and
// never below a working floor, and the stream counts only bound abuse.
const (
	h3MaxIncomingStreams = 8
)

func h3InitialStreamReceiveWindowByteCount() ByteCount     { return kib(256) }
func h3InitialConnectionReceiveWindowByteCount() ByteCount { return kib(512) }

func h3MaxStreamReceiveWindowByteCount() ByteCount {
	return MemoryScaledByteCount(mib(3), kib(384))
}

func h3MaxConnectionReceiveWindowByteCount() ByteCount {
	return MemoryScaledByteCount(mib(4), kib(512))
}

// applyH3MemoryBounds pins the receive windows and the incoming stream counts on
// a quic.Config. The platform transport uses one bidirectional stream the client
// opens, so the stream counts only bound a peer that tries to open more.
func applyH3MemoryBounds(config *quic.Config) {
	config.InitialStreamReceiveWindow = uint64(h3InitialStreamReceiveWindowByteCount())
	config.MaxStreamReceiveWindow = uint64(h3MaxStreamReceiveWindowByteCount())
	config.InitialConnectionReceiveWindow = uint64(h3InitialConnectionReceiveWindowByteCount())
	config.MaxConnectionReceiveWindow = uint64(h3MaxConnectionReceiveWindowByteCount())
	config.MaxIncomingStreams = h3MaxIncomingStreams
	config.MaxIncomingUniStreams = h3MaxIncomingStreams
}

var (
	h3DatagramLimitsOnce     sync.Once
	h3DatagramLimitsSettings *H3DatagramSettings
	h3DatagramLimitsBudget   *H3DatagramReassemblyBudget
)

// h3DatagramLimits returns the datagram settings and the shared reassembly
// budget. The process-wide reassembly allowance scales with the memory budget
// like upstream's (8 MiB, floor 512 KiB), so it is resolved on first use, which
// is the first H3 connection and therefore after the provider set its budget. A
// later change of the budget does not resize it.
func h3DatagramLimits() (*H3DatagramSettings, *H3DatagramReassemblyBudget) {
	h3DatagramLimitsOnce.Do(func() {
		settings := DefaultH3DatagramSettings()
		settings.ProcessReassemblyByteCount = int64(MemoryScaledByteCount(mib(8), kib(512)))
		h3DatagramLimitsSettings = settings
		h3DatagramLimitsBudget = NewH3DatagramReassemblyBudget(settings.ProcessReassemblyByteCount)
	})
	return h3DatagramLimitsSettings, h3DatagramLimitsBudget
}
