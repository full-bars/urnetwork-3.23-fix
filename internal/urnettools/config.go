package urnettools

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

func newConfigCmd() *cobra.Command {
	return withHelp(newCobraCmd("config", "show all provider settings with source and age", nil, func(cmd *cobra.Command, args []string) error {
		return cmdConfig(args)
	}), "Show all provider settings with their source and age from the control socket. Pass --json for raw JSON output.", "  urnet-tools config\n  urnet-tools config --json")
}

func cmdConfig(args []string) error {
	return runConfig(os.Stdout, args)
}

func runConfig(out io.Writer, args []string) error {
	jsonMode := false

	// Use lenient target parsing — unknown flags (like --json) are preserved
	// in rest, not rejected.
	t, rest, err := parseTargetFlagsLenient(args)
	if err != nil {
		return err
	}

	// Check rest for config-specific flags.
	for _, a := range rest {
		switch a {
		case "--json", "-j", "--json=true":
			jsonMode = true
		default:
			return fmt.Errorf("unknown argument: %s", a)
		}
	}

	var resp controlResponse
	hasSelector := t.Unit != "" || t.User != "" || t.Network != "" || t.NetworkID != "" || t.StateDir != ""
	if hasSelector {
		// Explicit target flags — resolve via provider discovery.
		p, err := selectTarget(Discover(), t)
		if err != nil {
			return err
		}
		if p.StateDir == "" {
			return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
		}
		sockPath := filepath.Join(p.StateDir, "provider.sock")
		resp, err = sendSocketRequest(sockPath, controlRequest{Cmd: "status"})
	} else {
		// No target flags — use default socket path.
		resp, err = dialControlSocket(controlRequest{Cmd: "status"})
	}
	if err != nil {
		return err
	}
	if !resp.OK {
		if resp.Error != "" {
			return fmt.Errorf("provider returned error: %s", resp.Error)
		}
		return fmt.Errorf("provider returned error")
	}

	if jsonMode {
		if len(resp.Raw) > 0 {
			raw := strings.TrimRight(string(resp.Raw), "\r\n")
			fmt.Fprintln(out, raw)
			return nil
		}
		data, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(data))
		return nil
	}

	keys := make([]string, 0, len(resp.Settings))
	for k := range resp.Settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	sources := make(map[string]struct{})
	type tableRow struct {
		setting string
		value   string
		source  string
		since   string
	}
	rows := make([]tableRow, 0, len(keys))

	wSetting := 16
	wValue := 8
	wSource := 9
	wSince := 10

	for _, k := range keys {
		info := resp.Settings[k]
		sources[info.Source] = struct{}{}

		since := "—"
		if info.SetAt != "" {
			if t, err := time.Parse(time.RFC3339, info.SetAt); err == nil && !t.IsZero() {
				since = formatRelativeTime(t)
			}
		}

		row := tableRow{
			setting: k,
			value:   info.Value,
			source:  info.Source,
			since:   since,
		}
		rows = append(rows, row)

		if rw := utf8.RuneCountInString(row.setting); rw > wSetting {
			wSetting = rw
		}
		if rw := utf8.RuneCountInString(row.value); rw > wValue {
			wValue = rw
		}
		if rw := utf8.RuneCountInString(row.source); rw > wSource {
			wSource = rw
		}
		if rw := utf8.RuneCountInString(row.since); rw > wSince {
			wSince = rw
		}
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "Setting\tValue\tSource\tSince")
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
		strings.Repeat("─", wSetting),
		strings.Repeat("─", wValue),
		strings.Repeat("─", wSource),
		strings.Repeat("─", wSince))

	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.setting, r.value, r.source, r.since)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "%d settings from %d sources\n", len(resp.Settings), len(sources))
	return nil
}

func formatRelativeTime(t time.Time) string {
	d := time.Since(t)
	if d < 0 {
		return "0s ago"
	}
	switch {
	case d < time.Minute:
		s := int(d.Round(time.Second).Seconds())
		if s >= 60 {
			return "1m ago"
		}
		return fmt.Sprintf("%ds ago", s)
	case d < time.Hour:
		m := int(d.Round(time.Minute).Minutes())
		if m >= 60 {
			return "1h ago"
		}
		return fmt.Sprintf("%dm ago", m)
	case d < 24*time.Hour:
		h := int(d.Round(time.Hour).Hours())
		if h >= 24 {
			return "1d ago"
		}
		return fmt.Sprintf("%dh ago", h)
	default:
		days := int(d.Hours() / 24)
		if days < 1 {
			days = 1
		}
		return fmt.Sprintf("%dd ago", days)
	}
}
