package connect

import (
	"os"
	"runtime/debug"
	"sync/atomic"

)

// ApplyAutoTuning runs once per proxy server, but the auto-profile summary is a
// global, identical line. autoTuneLogged gates it to one emission per process so
// a large proxy list does not spam the log on startup. autoTuneLogf is a test seam.
var (
	autoTuneLogged atomic.Bool
	autoTuneLogf   = func(format string, args ...any) { DefaultLogger().Infof(format, args...) }
)

// Tier Definitions
const (
	Tier1Low         = "lowmem"
	Tier2Balanced    = "balanced"
	Tier3Performance = "performance"
)

// ApplyAutoTuning detects the available system RAM and applies a performance
// tier profile to the client and NAT settings. This is an OPTIONAL profile
// triggered by URNETWORK_PROFILE=auto. It returns true if the dynamic
// Eco Memory Monitor should be started.
func ApplyAutoTuning(cs *ClientSettings, ns *LocalUserNatSettings) bool {
	if os.Getenv("URNETWORK_PROFILE") != "auto" {
		return false
	}

	ramLimit := DetectEffectiveRAMLimitBytes()
	tier := selectTier(ramLimit)

	if autoTuneLogged.CompareAndSwap(false, true) {
		autoTuneLogf("[tune] auto-profile: detected %d MiB RAM; applying '%s' settings", ramLimit/1024/1024, tier)
	}

	switch tier {
	case Tier1Low:
		applyTier1(cs, ns, ramLimit)
		return true // Enable dynamic GC monitoring
	case Tier2Balanced:
		applyTier2(cs, ns, ramLimit)
		return true // Enable dynamic GC monitoring
	case Tier3Performance:
		applyTier3(cs, ns)
	}

	return false
}

func selectTier(ramLimit int64) string {
	if ramLimit < 1200*1024*1024 { // 1.2 GiB
		return Tier1Low
	}
	if ramLimit < 3000*1024*1024 { // 3 GiB
		return Tier2Balanced
	}
	return Tier3Performance
}

func applyTier1(cs *ClientSettings, ns *LocalUserNatSettings, ramLimit int64) {
	// Contract Floor: 128 KiB
	cs.ContractManagerSettings.InitialContractTransferByteCount = kib(128)

	// IP Buffer Seq: 32
	ns.SequenceBufferSize = 32
	ns.TcpBufferSettings.SequenceBufferSize = 32
	ns.UdpBufferSettings.SequenceBufferSize = 32

	// TCP Max Window: 128 KiB
	ns.TcpBufferSettings.MaxWindowSize = uint32(kib(128))

	// WebRTC Recv: 512 KiB
	cs.WebRtcSettings.ReceiveBufferSize = kib(512)

	// Resend/Recv Queues: 512 KiB
	cs.SendBufferSettings.ResendQueueMaxByteCount = kib(512)
	cs.ReceiveBufferSettings.ReceiveQueueMaxByteCount = kib(512)

	// GOGC: 50
	debug.SetGCPercent(50)

	// GOMEMLIMIT: 85%
	debug.SetMemoryLimit(int64(float64(ramLimit) * 0.85))
}

func applyTier2(cs *ClientSettings, ns *LocalUserNatSettings, ramLimit int64) {
	// Contract Floor: 256 KiB
	cs.ContractManagerSettings.InitialContractTransferByteCount = kib(256)

	// IP Buffer Seq: 128
	ns.SequenceBufferSize = 128
	ns.TcpBufferSettings.SequenceBufferSize = 128
	ns.UdpBufferSettings.SequenceBufferSize = 128

	// TCP Max Window: 512 KiB
	ns.TcpBufferSettings.MaxWindowSize = uint32(kib(512))

	// WebRTC Recv: 1 MiB
	cs.WebRtcSettings.ReceiveBufferSize = mib(1)

	// Resend/Recv Queues: 1 MiB
	cs.SendBufferSettings.ResendQueueMaxByteCount = mib(1)
	cs.ReceiveBufferSettings.ReceiveQueueMaxByteCount = mib(1)

	// GOGC: 75
	debug.SetGCPercent(75)

	// GOMEMLIMIT: 90%
	debug.SetMemoryLimit(int64(float64(ramLimit) * 0.90))
}

func applyTier3(cs *ClientSettings, ns *LocalUserNatSettings) {
	cs.ContractManagerSettings.InitialContractTransferByteCount = mib(2)

	ns.SequenceBufferSize = 256
	ns.TcpBufferSettings.SequenceBufferSize = 256
	ns.UdpBufferSettings.SequenceBufferSize = 256

	ns.TcpBufferSettings.MaxWindowSize = uint32(mib(4))

	cs.WebRtcSettings.ReceiveBufferSize = mib(4)

	cs.SendBufferSettings.ResendQueueMaxByteCount = mib(4)
	cs.ReceiveBufferSettings.ReceiveQueueMaxByteCount = mib(4)
}
