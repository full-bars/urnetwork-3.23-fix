package main

import "testing"

func TestIsImportantLogLine(t *testing.T) {
	important := []string{
		"0625 01:18:34 [profit] earning=no reason=idle clients=0 rate=0 B/s proxies_up=12 serving=0 idle=12",
		"0625 01:19:19 [earn] billable_1m=0 B billable_60m=119 KB active=no",
		"0625 01:20:00 [health] uptime=5m ...",
		"[outage] backend degraded",
		"[pace] ✓ warmup: 12/12 up — done",
		"proxy[437] client_id=019e... connected",
		"instance_id=019e...",
		"[proxy][init] proxy[1] (1.2.3.4:1080) ... Permanently removed after 10 give-ups, will not be retried.",
		"[proxy][authrate] limiter pinned at floor",
	}
	for _, l := range important {
		if !isImportantLogLine(l) {
			t.Errorf("expected important: %q", l)
		}
	}

	noise := []string{
		"  proxy[753] 94.198.218.123:1080 (<no user>/<no password>)",
		"0625 01:18:37 [proxy][init] proxy[7316] (1.2.3.4:1) auth failed (attempt 2/3): proxy unreachable. Will retry in 2.48s",
		"I0625 01:18:47 [net][s]select: proxy[437] (..) success=3 dur=728ms",
		"pool[2048] tag=0 [] r=1/t=1/c=0 = 100% return",
		"0625 01:18:34 [proxy] reloaded: +989 added, -0 removed",
	}
	for _, l := range noise {
		if isImportantLogLine(l) {
			t.Errorf("expected NOT important: %q", l)
		}
	}
}
