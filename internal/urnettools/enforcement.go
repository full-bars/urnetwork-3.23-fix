package urnettools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// enforcement.go is the operator's view of what the governors did on a box: the
// durable record the provider keeps in its state dir (enforcement.jsonl, one
// line per event, and enforcement-daily.json, counts per UTC day). It reads the
// files directly, so it works while the provider is stopped and needs no new
// control-socket command.

const (
	enforcementDefaultLimit = 20
	enforcementMaxLimit     = 500
	enforcementLogFile      = "enforcement.jsonl"
	enforcementDailyFile    = "enforcement-daily.json"
)

// enforcementRow mirrors one line of the provider's enforcement.jsonl.
type enforcementRow struct {
	Time string `json:"ts"`
	Kind string `json:"kind"`
	Mode string `json:"mode"`
	Line string `json:"line"`
}

// enforcementDaily mirrors the provider's enforcement-daily.json.
type enforcementDaily struct {
	Total map[string]int            `json:"total"`
	Days  map[string]map[string]int `json:"days"`
}

func newEnforcementCmd() *cobra.Command {
	enforcementCmd := &cobra.Command{
		Use:                "enforcement <subcommand>",
		Short:              "show what the governors enforced on this box (counts per day, and the individual events)",
		Long:               "Show the durable record of what the governors enforced: releases of silent clients, connect budget cuts, attempt caps, memory holds and crash-loop starts. Mode 'shadow' means the governor only noted what it would have done. The files live in the provider's state dir and survive restarts and reboots.",
		Example:            "  urnet-tools enforcement summary\n  urnet-tools enforcement history 50",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			return cmdEnforcement(args)
		},
	}
	enforcementCmd.AddCommand(
		subPage("summary", "summary [target]", "counts per kind and mode (the default)",
			"Count the enforcement events per kind and mode for today, the last 7 and 30 UTC days, and since the log began. Mode 'shadow' means the governor only noted what it would have done.",
			"  urnet-tools enforcement summary --unit urnetwork-native.service",
			cmdEnforcement),
		subPage("history", "history [<N>] [target]", "list the newest enforcement events",
			"List the newest N enforcement events (default 20, at most 500), newest last. Releasing a silent client, a connect budget cut, an attempt cap, a memory hold and a crash-loop start all appear here; 'log' is an alias.",
			"  urnet-tools enforcement history 50 --unit urnetwork-native.service",
			cmdEnforcement),
	)
	return enforcementCmd
}

// parseEnforcementArgs returns the subcommand, the history limit and the target.
func parseEnforcementArgs(args []string) (sub string, limit int, t Target, err error) {
	t, rest, err := parseTargetFlags(args)
	if err != nil {
		return "", 0, t, err
	}
	sub, limit = "summary", enforcementDefaultLimit
	if len(rest) == 0 {
		return sub, limit, t, nil
	}
	switch rest[0] {
	case "summary":
		if len(rest) > 1 {
			return "", 0, t, fmt.Errorf("summary takes no arguments (got %q)", rest[1])
		}
	case "history", "log":
		sub = "history"
		if len(rest) > 2 {
			return "", 0, t, fmt.Errorf("usage: urnet-tools enforcement history [N]")
		}
		if len(rest) == 2 {
			n, perr := strconv.Atoi(rest[1])
			if perr != nil || n <= 0 {
				return "", 0, t, fmt.Errorf("N must be a positive integer (got %q)", rest[1])
			}
			limit = min(n, enforcementMaxLimit)
		}
	default:
		return "", 0, t, fmt.Errorf("unknown subcommand %q (usage: urnet-tools enforcement [summary|history [N]])", rest[0])
	}
	return sub, limit, t, nil
}

// enforcementSplit splits a "kind|mode" key.
func enforcementSplit(key string) (kind, mode string) {
	kind, mode, _ = strings.Cut(key, "|")
	return
}

// renderEnforcementSummary formats the counts as a table. now fixes "today" so
// the output is testable; windows are UTC dates, today included.
func renderEnforcementSummary(d enforcementDaily, now time.Time) string {
	keys := map[string]bool{}
	for k := range d.Total {
		keys[k] = true
	}
	for _, day := range d.Days {
		for k := range day {
			keys[k] = true
		}
	}
	if len(keys) == 0 {
		return "No enforcement events recorded yet.\n"
	}
	window := func(key string, days int) int {
		sum := 0
		for i := 0; i < days; i++ {
			sum += d.Days[now.UTC().AddDate(0, 0, -i).Format("2006-01-02")][key]
		}
		return sum
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)

	kindW, modeW := len("kind"), len("mode")
	for _, k := range ordered {
		kind, mode := enforcementSplit(k)
		kindW = max(kindW, len(kind))
		modeW = max(modeW, len(mode))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-*s  %-*s  %7s  %7s  %7s  %9s\n", kindW, "kind", modeW, "mode", "today", "7d", "30d", "lifetime")
	for _, k := range ordered {
		kind, mode := enforcementSplit(k)
		fmt.Fprintf(&b, "%-*s  %-*s  %7d  %7d  %7d  %9d\n", kindW, kind, modeW, mode,
			window(k, 1), window(k, 7), window(k, 30), d.Total[k])
	}
	b.WriteString("\n'shadow' rows are what the governor noted it would have done; only 'enforce' rows changed behaviour. Days are UTC.\n")
	return b.String()
}

// renderEnforcementHistory formats rows, newest last. Times are shown as
// recorded (UTC). The "[govern] " prefix is dropped: every row has it.
func renderEnforcementHistory(rows []enforcementRow) string {
	if len(rows) == 0 {
		return "No enforcement events recorded yet.\n"
	}
	modeW, kindW := 0, 0
	for _, r := range rows {
		modeW = max(modeW, len(r.Mode))
		kindW = max(kindW, len(r.Kind))
	}
	var b strings.Builder
	for _, r := range rows {
		ts := r.Time
		if t, err := time.Parse(time.RFC3339, r.Time); err == nil {
			ts = t.UTC().Format("2006-01-02 15:04:05Z")
		}
		fmt.Fprintf(&b, "%s  %-*s  %-*s  %s\n", ts, modeW, r.Mode, kindW, r.Kind, strings.TrimPrefix(r.Line, "[govern] "))
	}
	return b.String()
}

// enforcementAbsentNote returns the operator-facing note for a state dir that
// holds neither enforcement log file, or "" when at least one exists. An empty
// file is still a record (the provider creates the files it writes), so only a
// completely absent log gets the note.
func enforcementAbsentNote(stateDir string) string {
	_, errLog := os.Stat(filepath.Join(stateDir, enforcementLogFile))
	_, errDaily := os.Stat(filepath.Join(stateDir, enforcementDailyFile))
	if errLog == nil || errDaily == nil {
		return ""
	}
	return fmt.Sprintf("No enforcement log found in %s. The provider may be older than the version that records it, be using a different state dir, or be unable to write there.\n", stateDir)
}

// readEnforcementDaily reads the persisted counts; a missing file is empty.
func readEnforcementDaily(stateDir string) (enforcementDaily, error) {
	var d enforcementDaily
	b, err := os.ReadFile(filepath.Join(stateDir, enforcementDailyFile))
	if os.IsNotExist(err) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return d, fmt.Errorf("%s is unreadable: %w", enforcementDailyFile, err)
	}
	return d, nil
}

// readEnforcementTail returns the newest limit rows across the rotated file and
// the live one, oldest first. Lines that do not parse are skipped.
func readEnforcementTail(stateDir string, limit int) ([]enforcementRow, error) {
	var rows []enforcementRow
	for _, name := range []string{enforcementLogFile + ".1", enforcementLogFile} {
		f, err := os.Open(filepath.Join(stateDir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var r enforcementRow
			if json.Unmarshal(sc.Bytes(), &r) == nil && r.Kind != "" {
				rows = append(rows, r)
			}
		}
		f.Close()
	}
	if len(rows) > limit {
		rows = rows[len(rows)-limit:]
	}
	return rows, nil
}

func cmdEnforcement(args []string) error {
	sub, limit, t, err := parseEnforcementArgs(args)
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
	fmt.Printf("Governor enforcement for %s (%s)\n\n", providerLabel(p), p.StateDir)
	if note := enforcementAbsentNote(p.StateDir); note != "" {
		fmt.Print(note)
		return nil
	}
	if sub == "history" {
		rows, err := readEnforcementTail(p.StateDir, limit)
		if err != nil {
			return err
		}
		fmt.Print(renderEnforcementHistory(rows))
		return nil
	}
	d, err := readEnforcementDaily(p.StateDir)
	if err != nil {
		return err
	}
	fmt.Print(renderEnforcementSummary(d, time.Now()))
	return nil
}
