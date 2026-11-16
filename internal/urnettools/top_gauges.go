package urnettools

import (
	"fmt"

	"github.com/urnetwork/connect/internal/tui"
)

// drawGauge shows an unknown limit as a number, not an invented percentage.
func (m *topModel) drawGauge(b *tui.Buffer, y int, label, value string, amount, limit float64) {
	if y >= b.Height() || y < 0 {
		return
	}
	if limit <= 0 {
		m.kvAt(b, 9, y, label, value, tui.Style{})
		return
	}
	frac := amount / limit
	tui.DrawBar(b.Sub(tui.Rect{Y: y, W: b.Width(), H: 1}), tui.Bar{Label: label, LabelWidth: 8, Value: value, Frac: frac, LabelStyle: m.theme.Dim, Fill: m.theme.Level(frac, .7, .9), Empty: m.theme.Dim}, m.theme.ASCII)
}

// drawGauges is independent of the layout and hides readings not supplied.
func (m *topModel) drawGauges(b *tui.Buffer) {
	y := 0
	if m.snap != nil {
		r := m.snap.Resources
		if r.Threads != nil || r.TasksMax != nil {
			var threads, tasksMax uint64
			if r.Threads != nil {
				threads = *r.Threads
			}
			if r.TasksMax != nil {
				tasksMax = *r.TasksMax
			}
			text := fmt.Sprint(threads)
			if tasksMax > 0 {
				text = fmt.Sprintf("%d/%d", threads, tasksMax)
			}
			m.drawGauge(b, y, "threads", text, float64(threads), float64(tasksMax))
			y++
		}
		if r.OpenFDs != nil {
			limit := 0
			if r.FDLimit != nil {
				limit = *r.FDLimit
			}
			text := fmt.Sprint(*r.OpenFDs)
			if limit > 0 {
				text = fmt.Sprintf("%d/%d", *r.OpenFDs, limit)
			}
			m.drawGauge(b, y, "fds", text, float64(*r.OpenFDs), float64(limit))
			y++
		}
		if r.SwapTotalBytes != nil && *r.SwapTotalBytes > 0 && r.SwapUsedBytes != nil {
			used := float64(*r.SwapUsedBytes)
			total := float64(*r.SwapTotalBytes)
			text := topBytesPair(used, total)
			m.drawGauge(b, y, "swap", text, used, total)
			y++
		}
	}
	if in := m.rt.cur; in != nil {
		if in.HeapGoalBytes > 0 {
			m.drawGauge(b, y, "heap", topBytesPair(float64(in.HeapObjectsBytes), float64(in.HeapGoalBytes)), float64(in.HeapObjectsBytes), float64(in.HeapGoalBytes))
			y++
		}
		m.drawGauge(b, y, "gor", fmt.Sprint(in.Goroutines), float64(in.Goroutines), 0)
	}
}
