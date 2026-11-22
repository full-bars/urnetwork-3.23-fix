package urnettools

import (
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/tui"
)

// neonProxyCellStyle gives each proxy state its own neon hue: green for up,
// coral for dead, pale lime for degraded, blue for connecting and dim for
// parked. Up cells brighten with their throughput bucket.
func neonProxyCellStyle(th tui.Theme, state, bucket uint8) tui.Style {
	switch state {
	case connect.ProxyCellDead:
		return tui.Style{FG: th.Bad.FG, Attrs: tui.AttrBold}
	case connect.ProxyCellDegraded:
		return tui.Style{FG: th.Warn.FG}
	case connect.ProxyCellConnecting:
		return tui.Style{FG: th.SeriesV6FG}
	case connect.ProxyCellParked:
		return th.Dim
	}
	// Up: green ramp from dim to the neon green.
	ramp := []tui.Color{
		tui.Xterm(22), tui.Xterm(28), tui.Xterm(34), tui.Xterm(40),
		tui.Xterm(46), tui.Xterm(48), tui.Xterm(49), th.OK.FG,
	}
	if int(bucket) >= len(ramp) {
		bucket = uint8(len(ramp) - 1)
	}
	return tui.Style{FG: ramp[bucket]}
}
