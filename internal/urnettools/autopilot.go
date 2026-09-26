package urnettools

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// autopilot.go is the operator's view of the provider's capacity decisions: the
// OOM-aware start cap and trim results, recorded by the provider in
// ~/.urnetwork/autopilot.jsonl and served over the control socket as `ledger`.

const (
	autopilotDefaultLimit = 20
	autopilotMaxLimit     = 200
)

// ledgerRow mirrors the provider's ledger entry on the wire.
type ledgerRow struct {
	Time   string `json:"t"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	From   int    `json:"from"`
	To     int    `json:"to"`
	Mode   string `json:"mode,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func newAutopilotCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "autopilot log [limit] [target flags]",
		Short: "show the capacity decisions the provider made (OOM-aware cap, trim results)",
		Long: "Show the newest capacity decisions the provider recorded: the OOM-aware start cap " +
			"(shadow decisions are 'would' actions that were not enforced) and operator trim results, " +
			"newest last. The default is 20 entries, at most 200. Also prints the oom-cap control value; " +
			"set it with `urnet-tools set oom-cap on|off|shadow`.",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			return cmdAutopilot(args)
		},
	}
}

// parseAutopilotArgs extracts the subcommand ("log"), the limit and the target
// flags from the raw args.
func parseAutopilotArgs(args []string) (limit int, t Target, err error) {
	if len(args) == 0 {
		return 0, t, fmt.Errorf("usage: urnet-tools autopilot log [limit]")
	}
	if args[0] != "log" {
		return 0, t, fmt.Errorf("unknown subcommand %q (usage: urnet-tools autopilot log [limit])", args[0])
	}
	t, rest, err := parseTargetFlags(args[1:])
	if err != nil {
		return 0, t, err
	}
	limit = autopilotDefaultLimit
	if len(rest) > 0 {
		n, perr := strconv.Atoi(rest[0])
		if perr != nil || n <= 0 {
			return 0, t, fmt.Errorf("limit must be a positive integer (got %q)", rest[0])
		}
		limit = min(n, autopilotMaxLimit)
	}
	return limit, t, nil
}

// renderLedger formats the JSON array the provider returns as a timeline. Times
// are shown in UTC (as recorded) so the view does not depend on the machine's
// zone, and an unparseable time is shown as-is rather than dropping the entry.
func renderLedger(raw string) (string, error) {
	var rows []ledgerRow
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return "", fmt.Errorf("unreadable ledger reply: %w", err)
	}
	if len(rows) == 0 {
		return "No autopilot decisions recorded yet.\n", nil
	}
	var b strings.Builder
	actorW, actionW, changeW, modeW := 0, 0, 0, 0
	changes := make([]string, len(rows))
	for i, r := range rows {
		changes[i] = fmt.Sprintf("%d -> %d", r.From, r.To)
		actorW = max(actorW, len(r.Actor))
		actionW = max(actionW, len(r.Action))
		changeW = max(changeW, len(changes[i]))
		if r.Mode != "" {
			modeW = max(modeW, len(r.Mode)+2)
		}
	}
	for i, r := range rows {
		ts := r.Time
		if t, err := time.Parse(time.RFC3339, r.Time); err == nil {
			ts = t.UTC().Format("2006-01-02 15:04:05Z")
		}
		mode := ""
		if r.Mode != "" {
			mode = "[" + r.Mode + "]"
		}
		line := fmt.Sprintf("%s  %-*s  %-*s  %-*s  %-*s", ts, actorW, r.Actor, actionW, r.Action, changeW, changes[i], modeW, mode)
		if r.Reason != "" {
			line += "  " + r.Reason
		}
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	return b.String(), nil
}

func cmdAutopilot(args []string) error {
	limit, t, err := parseAutopilotArgs(args)
	if err != nil {
		return err
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	socketPath := filepath.Join(p.StateDir, "provider.sock")

	if resp, err := sendSocketRequest(socketPath, controlRequest{Cmd: "get", Key: "oom_cap"}); err == nil && resp.OK {
		v := "not set (default shadow)"
		if resp.Found && resp.Value != "" {
			v = resp.Value
		}
		fmt.Printf("oom-cap control value: %s (URNETWORK_OOM_CAP in the unit may also apply; any source saying off wins)\n\n", v)
	}

	resp, err := sendSocketRequest(socketPath, controlRequest{Cmd: "ledger", Limit: limit})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("provider returned error: %s (a provider older than this feature does not know the ledger command)", resp.Error)
	}
	out, err := renderLedger(resp.Value)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}
