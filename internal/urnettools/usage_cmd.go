package urnettools

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// newUsageCmd builds the `urnet-tools usage` command (cards) with the
// `usage graph[s]` subcommand for time-series views. All views read a single
// targeted provider's persistent usage_history.jsonl (written by the provider
// each hour) — no per-proxy enumeration, so it stays fast at any fleet size.
func newUsageCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "usage",
		Short: "show aggregate usage (billable vs control) across time spans",
		Long: "Show the targeted provider's aggregate traffic usage as summary cards\n" +
			"(billable vs control-plane split) and as day/hour/month time-series graphs.\n" +
			"Reads the provider's persistent usage_history.jsonl, so lifetime spans survive\n" +
			"restarts and updates.\n\n" +
			"  urnet-tools usage            # summary cards (default)\n" +
			"  urnet-tools usage graphs     # all three time-series\n" +
			"  urnet-tools usage graph day  # single view: day | hour | month",
		Example:            "  urnet-tools usage\n  urnet-tools usage graphs --unit mynetwork-provider\n  urnet-tools usage graph month --network tacogonzalez3000",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, a := range args {
				if a == "-h" || a == "--help" {
					return cmd.Help()
				}
			}
			return cmdUsage(args)
		},
	}
}

// resolveTargetUsage resolves a single provider target and returns its state dir.
func resolveTargetUsage(args []string) (Provider, []string, error) {
	t, rest, err := parseTargetFlagsLenient(args)
	if err != nil {
		return Provider{}, nil, err
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return Provider{}, nil, err
	}
	return p, rest, nil
}

// cmdUsage dispatches `usage` / `usage graph[s] <view>`.
func cmdUsage(args []string) error {
	if len(args) == 0 {
		// Bare `usage` — cards.
		return cmdUsageCards(nil)
	}

	// Scan for the subcommand (graph/graphs) regardless of flag position.
	// "usage --unit X graph day" should route to graph, not cards.
	for i, a := range args {
		switch a {
		case "graphs":
			// `usage graphs` = all three views; never treat a following
			// token (e.g. --unit) as a view.
			return cmdUsageGraph(args[:i], "")
		case "graph":
			// `usage graph <view>`: consume exactly one non-flag token as
			// the view; flags before/after still route to target parsing.
			var view string
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				view = args[i+1]
			}
			return cmdUsageGraph(args[:i], view)
		case "-h", "--help":
			return fmt.Errorf("usage: show aggregate usage (billable vs control)\n\n  urnet-tools usage\n  urnet-tools usage graphs\n  urnet-tools usage graph day|hour|month")
		}
	}

	// No subcommand found — treat all args as target flags and show cards.
	return cmdUsageCards(args)
}

// cmdUsageCards renders the summary cards for a targeted provider.
func cmdUsageCards(targetArgs []string) error {
	p, _, err := resolveTargetUsage(targetArgs)
	if err != nil {
		return err
	}
	stateDir := p.StateDir
	snaps := readUsageHistory(stateDir)
	if len(snaps) == 0 {
		fmt.Printf("No usage history yet for %s (state dir %s).\n", providerLabel(p), stateDir)
		fmt.Println("The provider writes an aggregate snapshot each hour; check back after it has run.")
		return nil
	}

	// The reference (latest) snapshot gives "since start" (cumulative-per-process,
	// which resets on restart). Lifetime is segment-summed from the history
	// (see usageLifetime): the totals can drop on ordinary proxy churn, so a
	// running max would discard post-drop growth.
	ref := snaps[len(snaps)-1]
	sinceStart := usageAggregates{BillableRX: ref.BillableRX, BillableTX: ref.BillableTX, TotalRX: ref.RX, TotalTX: ref.TX}
	lifetime := usageLifetime(snaps)
	now := time.Now()
	day := usageWindow(snaps, 24*time.Hour, now)
	week := usageWindow(snaps, 7*24*time.Hour, now)
	month := usageWindow(snaps, 30*24*time.Hour, now)

	fmt.Printf("URN usage — %s (%s)\n", providerLabel(p), p.netLabel())
	fmt.Println()
	renderUsageCard("LIFETIME", lifetime)
	renderUsageCard("LAST 30D", month)
	renderUsageCard("LAST 7D", week)
	renderUsageCard("LAST 24H", day)
	renderUsageCard("SINCE START", sinceStart)
	fmt.Println()
	fmt.Printf("  Lifetime billable share: %.1f%% of %s total\n",
		pct(lifetime.Billable(), lifetime.Total()), fmtBytes(lifetime.Total()))
	for title, agg := range map[string]usageAggregates{
		"LIFETIME": lifetime, "LAST 30D": month, "LAST 7D": week, "LAST 24H": day, "SINCE START": sinceStart,
	} {
		if agg.BillableExceedsTotal() {
			fmt.Fprintf(os.Stderr, "warning: %s: billable (%s) > total (%s) — independent counters (ip.go vs net.go) disagree; control traffic floored to 0\n",
				title, fmtBytes(agg.Billable()), fmtBytes(agg.Total()))
		}
	}
	fmt.Printf("  Updated with latest hourly snapshot %s\n", ref.TS.UTC().Format(time.RFC3339))
	return nil
}

// renderUsageCard draws one clean summary box: billable, control %, a mini
// stacked bar (billable vs control), total, and the billable:control ratio.
func renderUsageCard(title string, a usageAggregates) {
	// Fixed content width (inside the box borders).
	const cw = 39
	fmt.Printf("┌─ %-39s┐\n", title)
	fmt.Printf("│  BILLABLE  %s │\n", padRight(fmtBytes(a.Billable()), cw-11))
	pctCtl := pct(a.Control(), a.Total())
	fmt.Printf("│  CONTROL   %s │\n", padRight(fmt.Sprintf("%s (%.1f%%)", fmtBytes(a.Control()), pctCtl), cw-11))
	fmt.Printf("│  %s │\n", ratioBar(a.Billable(), a.Control()))
	fmt.Printf("│  TOTAL     %s │\n", padRight(fmtBytes(a.Total()), cw-11))
	fmt.Printf("│  ratio     %s │\n", padRight(ratioStr(a.Billable(), a.Control()), cw-11))
	fmt.Printf("└%s┘\n", strings.Repeat("─", 41))
	fmt.Println()
}

// padRight pads s to width w with spaces.
func padRight(s string, w int) string {
	if len(s) >= w {
		return s[:w]
	}
	return s + strings.Repeat(" ", w-len(s))
}

// ratioBar renders a 30-char bar of '=' (billable) and spaces (control),
// filling the card's content width.
func ratioBar(billable, control uint64) string {
	total := billable + control
	if total == 0 {
		return strings.Repeat(" ", 38)
	}
	// Content width: the card box is 43 chars wide; the bar row is
	// `│  %s │` (3 + bar + 2), so the bar must be 38 to align with the
	// BILLABLE/CONTROL/TOTAL value columns (was 30 -> 8 cells short).
	const w = 38
	b := int(float64(billable) / float64(total) * w)
	if b > w {
		b = w
	}
	return strings.Repeat("=", b) + strings.Repeat(" ", w-b)
}

func ratioStr(billable, control uint64) string {
	if control == 0 {
		return "∞ : 1"
	}
	return fmt.Sprintf("%.1f : 1", float64(billable)/float64(control))
}

func pct(part, whole uint64) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) * 100 / float64(whole)
}

func fmtBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := float64(unit), 0
	for n := float64(b) / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/div, "KMGTPE"[exp])
}
