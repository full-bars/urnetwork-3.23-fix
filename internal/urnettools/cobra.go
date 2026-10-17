package urnettools

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// rootHelpTemplate is the curated, sectioned root help for urnet-tools. It
// replaces Cobra's default flat "Available Commands" list with the grouped,
// emoji-style layout the operator approved in the legacy shell tool — now
// relaying every command the Go/Cobra build exposes. Per-command help is
// unaffected (each subcommand keeps its own Cobra help page on -h/--help).
// The support footer matches the legacy tool the user liked.
const rootHelpMenu = `urnet-tools — provider-aware URnetwork manager

Usage:
  urnet-tools [command]

Core Commands:
  start                   Start the provider
  stop                    Stop the provider
  restart [-y|-f]         Restart the provider (-y/-f to skip confirmation)
  update                  Upgrade to the latest version
  hotswap                 Swap provider process with no gap; proxies rebuild ~30s
  self-update             Update this tool binary itself
  status                  Show provider service status
  top [--interval D]      Live full-screen view of a provider (also: urtop)
  logs [target] [N]       Show recent logs, then follow (N lines, default 250)
  dashboard               Status panel: state, settings, sources, warnings
  history [limit]         Show the provider's command audit trail
  autopilot log [limit]   Show the capacity decisions the provider made (OOM-aware cap, trim)
  enforcement [history N] Show what the governors enforced (counts per day; or the newest N events)
  baseline [show|mark|compare]  Show this box's recorded behaviour, and compare an upgrade

Performance & Tuning:
  turbo <v4|v8|off>       RAISE throughput limits for RAM-rich boxes
  auto <on|off>           AUTO-TUNE detect hardware and pick best profile
  eco <on|off>            ECO MODE GC-tuned for low-RAM systems
  lowmode <on|off>        LOW-MEMORY reduced buffers for max RAM savings
  ramlogs <on|off>        RAM LOGS zero disk I/O logging
  optimize                Apply Golden Fleet OS/kernel limits
  set [<k> [<v>|off]]     Show or change runtime tuning overrides
  get [key] [target]      Show the current value of a runtime override (alias: show)
  fast-auth [on|off]      Bypass auth rate limiter without restart
  config [--json]         Show all provider settings with source and age
  profile [<name>]        Show or set the memory/GC tuning profile
  metrics [on|off|listen] Prometheus /metrics: status, toggle, listen address
  smart-dialer [on|off]   Adapt transport choice to the network: skip DPI-circumvention cost when it is not needed

Session & Identity:
  session save <file>     Export identity + proxy state (encrypted)
  session load <file>     Import identity + proxy state, then restart
  auth [<code>]           Authenticate (omit for interactive paste)
  sn-status [--json]      Subnet 25 node rank, coldkey & miner status
  choose-network          Set API/connect endpoints
  default                 Persist a default provider target for this box
  rename <name>           Set dashboard display name (alias: set node-name)

Proxy Management:
  proxy add <file>        Bulk add proxies from a text file
  proxy paste             Paste raw proxies from stdin, file, or URL
  proxy clear             Remove all configured proxies
  proxy remove            Remove proxies (by addr/match, or all)
  proxy add-source <url>  Add a URL proxy source (fetched + cached)
  proxy remove-source <url> Remove a URL proxy source
  proxy refresh [--force] Re-read configs and hot-reload proxies
  proxy trim <N>          Hold running proxies at N, shed worst first
  proxy health            Show dead/degraded proxies + live event log
  proxy traffic           Real-time bandwidth & client session load
  proxy ids               Client IDs for each proxy (from JWT store)
  summary                 Fleet-style proxy summary (sources, health, counts)
  proxy remove-dead       Prune dead/degraded/failing proxies interactively
  self-heal [on|off]      Auto-regulate proxies (load gate + cleanup)
  show-ip [on|off|status] Show public IP on the dashboard label (was: ip-detect)
  direct [on|off]         Toggle providing on the machine's direct/local IP
  usage [graph[s] <view>] Traffic accounting: billable vs control, time-series

Wallet Consent:
  wallet status           Show the network wallet consent (read-only)
  wallet challenge <ck>   Fetch the consent statement to sign (signs nothing)
  wallet set <ck>         Submit a signed consent (--coldkey_seed_file: own workstation only)
  wallet sign <ck>        Sign in the browser (Talisman); the seed never leaves the extension

Maintenance:
  reinstall                     Reinstall provider
  uninstall                     Uninstall provider
  auto-update                   Manage auto-update (--interval daily|weekly|monthly)
  auto-start                    Toggle auto-start on login
  providers                     List all providers on this box

Info:
  version                       Print this tool's version
  help <command>                Show help for a command

Targeting (used when the box runs more than one provider):
  --unit <unit>             systemd unit, e.g. urnetwork-native.service
  --user <user>             OS user, e.g. urnet
  --network <name>          JWT network name (account identity)
  --network-id <id>         JWT network id (true unique identity)
  --state-dir <dir>         state dir

Batch / safety flags:
  -f, --force                 skip confirmation prompts ONLY (never picks providers)
  -n, --dry-run               print the plan, change nothing
  -h, --help                  show help (never executes)

Need help? Email support@fullbars.xyz or visit https://github.com/full-bars/urnetwork-3.23-fix
`

// buildRootCmd creates the root Cobra command for urnet-tools.
func buildRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:           "urnet-tools",
		Short:         "provider-aware URnetwork manager",
		Long:          "urnet-tools — provider-aware URnetwork manager",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	rootCmd.SetOut(os.Stderr)
	rootCmd.SetErr(os.Stderr)
	// Restore the curated sectioned root menu (the operator-approved layout)
	// WITHOUT touching per-command help: the root's own Run and Help print the
	// menu; every subcommand keeps Cobra's default per-command -h/--help page.
	// Using SetHelpTemplate here would CASCADE to subcommands and break their
	// per-command help pages, so instead we bind the menu to the root only.
	rootCmd.Run = func(cmd *cobra.Command, args []string) {
		fmt.Fprint(cmd.OutOrStderr(), rootHelpMenu)
	}
	// Root -h/--help renders the same curated menu. Subcommands reset their own
	// help template so they keep Cobra's per-command page (see newCobraCmd).
	rootCmd.SetHelpTemplate(rootHelpMenu)
	// Shell completion: the modern cobra completion command (bash/zsh/fish/
	// powershell) is registered, so `urnet-tools completion <shell>` works.
	rootCmd.CompletionOptions.DisableDefaultCmd = false

	// No persistent flags: every subcommand hand-parses its own flags with
	// DisableFlagParsing, so cobra flags existed only to render an inherited
	// "Global Flags" block on every page. Each command declares the flags it
	// honors through attachGlobalHelpFlags below; the root page's curated
	// menu documents the shared flags for readers.

	rootCmd.AddCommand(
		newProvidersCmd(),
		newStatusCmd(),
		newTopCmd(),
		newSnStatusCmd(),
		newStartCmd(),
		newStopCmd(),
		newRestartCmd(),
		newUpdateCmd(),
		newIdleUpdateCmd(),
		newSelfUpdateCmd(),
		newLogsCmd(),
		newSummaryCmd(),
		newVersionCmd(),
		newDefaultCmd(),
		newSessionCmd(),
		newTurboCmd(),
		newAutoCmd(),
		newEcoCmd(),
		newLowmodeCmd(),
		newRamlogsCmd(),
		newOptimizeCmd(),
		newHotswapCmd(),
		newFastAuthCmd(),
		newSetCmd(),
		newAuthCmd(),
		newChooseNetworkCmd(),
		newWalletCmd(),
		newProxyCmd(),
		newDirectCmd(),
		newUsageCmd(),
		newReinstallCmd(),
		newUninstallCmd(),
		newAutoUpdateCmd(),
		newAutoStartCmd(),
		newSelfHealCmd(),
		newDoRestartCmd(), // HIDDEN internal entry point for the updater's escalated restart
		newIPDetectCmd(),
		newRenameCmd(),
		newGetCmd(),
		newHistoryCmd(),
		newAutopilotCmd(),
		newEnforcementCmd(),
		newBaselineCmd(),
		newMetricsCmd(),
		newSmartDialerCmd(),
		newProfileCmd(),
		newDashboardCmd(),
		newConfigCmd(),
	)
	// Force every subcommand (however it was constructed) back to Cobra's
	// default per-command help page. The root's curated menu must only ever
	// show for bare `urnet-tools` / root -h; without this reset the root's
	// custom help template would cascade down to subcommand -h/--help pages.
	for _, sub := range rootCmd.Commands() {
		// Restore Cobra's default per-command help page (the framework's
		// private defaultHelpTemplate). The root's curated menu must show only
		// for bare `urnet-tools` / root -h, never for a subcommand's -h.
		sub.SetHelpTemplate(`{{with (or .Long .Short)}}{{. | trimTrailingWhitespaces}}
{{end}}{{if or .Runnable .HasSubCommands}}{{.UsageString}}{{end}}`)
	}

	attachGlobalHelpFlags(rootCmd)

	return rootCmd
}

// globalFlagMatrix records, per command, which shared flag sets its handler
// honors: t = the targeting flags (--unit/--user/--network/--network-id/
// --state-dir), s = the safety flags (-f/--force, -n/--dry-run). Derived from
// the handlers: t where the chain parses targets, s where it parses the
// global flags. Wallet subcommands are keyed "wallet/<name>".
var globalFlagMatrix = map[string]string{
	"auto": "ts", "auto-start": "ts", "auto-update": "ts", "eco": "ts", "get": "ts",
	"hotswap": "ts", "idle-update": "ts", "logs": "ts", "lowmode": "ts", "metrics": "ts",
	"optimize": "ts", "ramlogs": "ts", "reinstall": "ts", "rename": "ts", "restart": "ts",
	"set": "ts", "smart-dialer": "ts", "sn-status": "ts", "start": "ts", "status": "ts",
	"stop": "ts", "top": "ts", "turbo": "ts", "uninstall": "ts", "update": "ts",
	"direct": "ts", "fast-auth": "ts", "session": "ts",
	"auth": "t", "autopilot": "t", "baseline": "t", "choose-network": "t", "config": "t",
	"dashboard": "t", "default": "t", "enforcement": "t", "history": "t", "profile": "t",
	"summary": "t", "usage": "t",
	"providers": "s", "self-update": "s",
	"wallet/status": "t", "wallet/challenge": "t", "wallet/set": "t", "wallet/sign": "t",
	"proxy":     "ts",
	"proxy/add": "ts", "proxy/paste": "ts", "proxy/clear": "ts", "proxy/remove": "ts",
	"proxy/refresh": "ts", "proxy/add-source": "ts", "proxy/remove-source": "ts",
	"proxy/health": "ts", "proxy/traffic": "ts", "proxy/ids": "ts",
	"proxy/remove-dead": "ts", "proxy/trim": "ts", "proxy/audit": "ts",
	"session/save": "ts", "session/load": "ts",
	"metrics/status": "ts", "metrics/on": "ts", "metrics/off": "ts", "metrics/listen": "ts",
	"default/set": "t", "default/show": "t", "default/clear": "t",
	"baseline/show": "t", "baseline/mark": "t", "baseline/compare": "t",
	"enforcement/summary": "t", "enforcement/history": "t",
	"autopilot/log": "t",
}

// attachGlobalHelpFlags declares the shared flag sets on the commands that
// honor them, so a page shows the flags it actually accepts instead of a
// universal block.
func attachGlobalHelpFlags(root *cobra.Command) {
	for _, sub := range root.Commands() {
		switch globalFlagMatrix[sub.Name()] {
		case "ts":
			withTargetAndSafetyFlags(sub)
		case "t":
			withTargetFlags(sub)
		case "s":
			withSafetyFlags(sub)
		}
		for _, subsub := range sub.Commands() {
			switch globalFlagMatrix[sub.Name()+"/"+subsub.Name()] {
			case "ts":
				withTargetAndSafetyFlags(subsub)
			case "t":
				withTargetFlags(subsub)
			case "s":
				withSafetyFlags(subsub)
			}
		}
	}
}

func newCobraCmd(use, short string, aliases []string, handler func(cmd *cobra.Command, args []string) error) *cobra.Command {
	return &cobra.Command{
		Use:                use,
		Short:              short,
		Aliases:            aliases,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			return handler(cmd, args)
		},
	}
}

func parseGlobal(args []string, handler func(force, dryRun bool, rest []string) error) error {
	force, dryRun, rest, err := parseGlobalFlags(args)
	if err == errHelpShown {
		return nil
	}
	if err != nil {
		return err
	}
	return handler(force, dryRun, rest)
}

// subPage builds a child page for a group command whose dispatcher takes the
// raw subcommand args: the page prepends its own name and calls the same
// dispatcher the parent uses, so behaviour is unchanged.
func subPage(sub, use, short, long, example string, dispatch func([]string) error) *cobra.Command {
	return withHelp(newCobraCmd(use, short, nil, func(cmd *cobra.Command, args []string) error {
		return dispatch(append([]string{sub}, args...))
	}), long, example)
}

// subPageTS is subPage for groups whose dispatcher also takes the -f/--force
// and -n/--dry-run flags, routed through parseGlobal exactly like the parent.
func subPageTS(sub, use, short, long, example string, dispatch func(force, dryRun bool, rest []string) error) *cobra.Command {
	return withHelp(newCobraCmd(use, short, nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(append([]string{sub}, args...), dispatch)
	}), long, example)
}

func newProvidersCmd() *cobra.Command {
	return withFlags(withHelp(newCobraCmd("providers [--all]", "list providers on this box", []string{"list", "ps"}, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdProviders(rest)
		})
	}), "List providers found on this box (systemd units and bare processes) with their JWT network identity. An unprivileged caller sees only the providers owned by their own OS user (the one-provider-per-user contract); --all as root lists every user's. If no systemd providers exist but containers do, it says so and points at urnet-docker.", "  urnet-tools providers\n  urnet-tools providers --all"),
		helpFlag{Name: "all", Usage: "include providers owned by other OS users (run as root)"},
	)
}

func newStatusCmd() *cobra.Command {
	return withFlags(withHelp(newCobraCmd("status [target]", "detailed status of one provider", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdStatus(rest)
		})
	}), "Show detailed status for one provider: unit, binary, version, state dir, PID, running state, network identity, JWT expiry, and a live block with throughput, clients, proxies, uptime, last restart reason and memory when the provider answers. With several providers and no target it prints one compact row each; --json prints the raw snapshot for scripts.", "  urnet-tools status\n  urnet-tools status --network mynetwork\n  urnet-tools status --unit urnetwork-native.service\n  urnet-tools status --json"),
		helpFlag{Name: "json", Usage: "print the raw live snapshot as JSON"},
	)
}

func newTopCmd() *cobra.Command {
	return withFlags(withHelp(newCobraCmd("top [target]", "live full-screen view of a provider", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdTop(rest)
		})
	}), "Open a live, full-screen view of one provider: throughput graphs for the last 10 minutes, current and average rates, clients, proxy pool, memory and recent events. It reads the control socket and changes nothing, so it is safe to leave open. Needs an interactive terminal; use status for scripts.\n\nKeys:\n  q, Esc, Ctrl-C   quit\n  Tab, Shift-Tab   switch provider\n  - / +            slower / faster refresh\n  w                zoom the graphs to the last 15 seconds\n  g                show where the provider's goroutines are parked\n  m                color theme and graph style (remembered)\n  ?                help\n\nA slow provider shows SLOW with the last data; a silent one shows DISCONNECTED with a countdown and resumes by itself. The snapshot refreshes at most once a second; --interval sets the graph and rate refresh period.", "  urnet-tools top\n  urnet-tools top --network mynetwork\n  urnet-tools top --interval 500ms\n  urtop"),
		helpFlag{Name: "interval", Value: "DURATION", Usage: "refresh period such as 500ms or 2s (default 1s, minimum 100ms)"},
	)
}

func newSnStatusCmd() *cobra.Command {
	return withFlags(withHelp(newCobraCmd("sn-status [target]", "Subnet 25 node rank, coldkey & miner status", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdSnStatus(rest)
		})
	}), "Show real-time Subnet 25 metrics: global network rank, billable bandwidth, registered Bittensor coldkey, Top 200 cutoff eligibility, contract clock, and payout share. Supports --json for machine-readable automation. Target a specific provider with --unit, --user, --network, --network-id, or --state-dir.", "  urnet-tools sn-status\n  urnet-tools sn-status --json\n  urnet-tools sn-status --unit urnetwork-native.service"),
		helpFlag{Name: "json", Shorthand: "j", Usage: "print machine-readable JSON"},
	)
}

func newStartCmd() *cobra.Command {
	return withHelp(newCobraCmd("start [target]", "start the provider's systemd unit", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdStart(rest, force, dryRun)
		})
	}), "Start the provider's systemd unit. Target a specific provider with --unit, --user, --network, --network-id, or --state-dir. Honors --dry-run, which prints the plan and starts nothing.", "  urnet-tools start\n  urnet-tools start --unit urnetwork-native.service\n  urnet-tools start --user urnet --dry-run")
}

func newStopCmd() *cobra.Command {
	return withHelp(newCobraCmd("stop [target]", "stop the provider's systemd unit", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdStop(rest, force, dryRun)
		})
	}), "Stop the provider's systemd unit. Target a specific provider with --unit, --user, --network, --network-id, or --state-dir. Honors --dry-run, which prints the plan and stops nothing.", "  urnet-tools stop\n  urnet-tools stop --unit urnetwork-native.service")
}

func newRestartCmd() *cobra.Command {
	return withHelp(newCobraCmd("restart [target]", "restart the provider's systemd unit", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdRestart(rest, force, dryRun)
		})
	}), "Restart the provider's systemd unit. This is a production action, so it asks for a typed \"yes\" unless you pass -f/--force (or -y/--yes). Use -n/--dry-run to print the plan without acting.", "  urnet-tools restart --unit urnetwork-native.service\n  urnet-tools restart --network mynetwork --force")
}

func newUpdateCmd() *cobra.Command {
	return withFlags(withHelp(newCobraCmd("update [target]", "update provider(s) to latest", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdUpdate(rest, force, dryRun)
		})
	}), "Download and install the latest provider release: verify the sha256, swap the binary, restart the owning unit, and refresh the urnet-tools binary itself from the same release. With no target and several providers it prompts; --all updates every provider, --include/--exclude pick a subset, and --tag/--digest/--url pin a release.", "  urnet-tools update\n  urnet-tools update --unit urnetwork-native.service\n  urnet-tools update --all --force\n  urnet-tools update --tag v3.23.0-fix.30.5"),
		helpFlag{Name: "tag", Value: "TAG", Usage: "install the release `TAG` instead of the latest"},
		helpFlag{Name: "digest", Value: "SHA256", Usage: "install an exact asset after verifying its sha256 `SHA256`"},
		helpFlag{Name: "url", Value: "URL", Usage: "fetch the release asset from this `URL`"},
		helpFlag{Name: "include", Value: "LABELS", Usage: "update only providers matching these comma-separated `LABELS`"},
		helpFlag{Name: "exclude", Value: "LABELS", Usage: "skip providers matching these comma-separated `LABELS`"},
		helpFlag{Name: "all", Usage: "update every provider on this box without prompting"},
	)
}

func newIdleUpdateCmd() *cobra.Command {
	long := `Wait for billable traffic to drop below --threshold for a sustained --window before applying an update, so active client connections are not severed mid-flight. A --timeout ceiling forces the update when traffic never fully quiets: it logs a notice and proceeds. Pass --tag to pin a specific release.`

	examples := `  # Wait for up to 30m for a 5-minute quiet window below 5 KiB/s:
  urnet-tools idle-update

  # Target a specific systemd unit with custom 10 KiB/s threshold and 1-hour timeout:
  urnet-tools idle-update --unit urnetwork-native.service --threshold 10240 --window 10m --timeout 1h

  # Immediate update bypass (equivalent to 'update' but through idle-update interface):
  urnet-tools idle-update --window 0

  # Preview the wait parameters and target release without acting:
  urnet-tools idle-update --dry-run`

	return withFlags(withHelp(newCobraCmd("idle-update [target]", "wait for traffic lull before updating provider(s)", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdIdleUpdate(rest, force, dryRun)
		})
	}), long, examples),
		helpFlag{Name: "threshold", Value: "RATE", Usage: "max billable throughput in bytes/s to count as idle (default 5120)"},
		helpFlag{Name: "window", Value: "DURATION", Usage: "how long traffic must stay quiet, seconds or a duration such as 5m (default 5m, 0 = immediate)"},
		helpFlag{Name: "timeout", Value: "DURATION", Usage: "give up waiting and update anyway, seconds or a duration such as 30m (default 30m, 0 = wait forever)"},
		helpFlag{Name: "tag", Value: "TAG", Usage: "install the release `TAG` instead of the latest"},
	)
}

func newSelfUpdateCmd() *cobra.Command {
	return withFlags(withHelp(newCobraCmd("self-update", "update this tool binary itself", []string{"selfupdate"}, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdSelfUpdate(rest, force, dryRun)
		})
	}), "Update only the urnet-tools binary itself to the latest release, verifying its sha256 digest before swapping it in. No provider is touched or restarted. Pin a version with --tag, or point at an exact asset with --digest and --url.", "  urnet-tools self-update\n  urnet-tools self-update --tag v3.23.0-fix.30.5\n  urnet-tools self-update --force"),
		helpFlag{Name: "tag", Value: "TAG", Usage: "install the release `TAG` instead of the latest"},
		helpFlag{Name: "digest", Value: "SHA256", Usage: "install an exact asset after verifying its sha256 `SHA256`"},
		helpFlag{Name: "url", Value: "URL", Usage: "fetch the release asset from this `URL`"},
	)
}

func newLogsCmd() *cobra.Command {
	return withHelp(newCobraCmd("logs [target] [N]", "show recent provider logs", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdLogs(rest)
		})
	}), "Show recent provider logs and then follow them. The default is 250 lines; pass a number to change it. If the unit runs with RAMLOGS or a low-memory profile, this streams from the RAM log buffer instead of journald.", "  urnet-tools logs\n  urnet-tools logs --unit urnetwork-native.service 200")
}

func newSummaryCmd() *cobra.Command {
	return withHelp(newCobraCmd("summary [target]", "fleet-style summary for one provider", nil, func(cmd *cobra.Command, args []string) error {
		rest, err := parseDelegationArgs(args)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		return cmdSimpleDelegation("summary", rest)
	}), "Show a fleet-style activity and performance summary for one provider. This delegates to the provider binary's own proxy summary command, so it needs the provider binary to be reachable and running.", "  urnet-tools summary\n  urnet-tools summary --network mynetwork")
}

func newVersionCmd() *cobra.Command {
	// '-v'/'--version' are handled by the top-level dispatcher, not here:
	// Cobra strips '-' tokens before alias matching, so they never resolve as
	// aliases. They print the bare tool version. Plain 'version' reports the
	// provider inventory as well, and shares its implementation with the
	// dispatcher so the two entry points cannot answer differently.
	return withHelp(newCobraCmd("version", "print this tool's version and each provider's", nil, func(cmd *cobra.Command, args []string) error {
		printToolVersionAndProviders()
		return nil
	}), "Print the urnet-tools build version, then every discovered provider and the version it is actually running. This is what to verify an upgrade with, rather than the exit status of 'update'. Use -v for the tool's own version alone.", "  urnet-tools version")
}

func newDefaultCmd() *cobra.Command {
	defaultCmd := withHelp(newCobraCmd("default <subcommand>", "persist a default provider target for this box", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDefault(args)
	}), "Persist, show, or clear a default provider target for this box, so later commands with no explicit target resolve to it on multi-provider boxes. 'default set' requires exactly one of --unit, --user, --network, --network-id, or --state-dir; the default is stored per user and is ignored automatically if it becomes stale or ambiguous.", "  urnet-tools default set --network mynetwork\n  urnet-tools default show\n  urnet-tools default clear")
	defaultCmd.AddCommand(
		subPage("set", "set", "persist a default provider target",
			"Persist the default provider target for this box. Requires exactly one of --unit, --user, --network, --network-id, or --state-dir. The default is stored per user and ignored automatically when it becomes stale or ambiguous.",
			"  urnet-tools default set --network mynetwork",
			cmdDefault),
		subPage("show", "show", "show the stored default target",
			"Show the default provider target stored for this user, if any, and what it resolves to right now.",
			"  urnet-tools default show",
			cmdDefault),
		subPage("clear", "clear", "clear the stored default target",
			"Clear the stored default provider target for this user, so commands go back to requiring an explicit target on multi-provider boxes.",
			"  urnet-tools default clear",
			cmdDefault),
	)
	return defaultCmd
}

func newSessionCmd() *cobra.Command {
	// cmdSession owns its rich help (save|load <file>, --allow-different-account);
	// building raw here lets that help fire instead of Cobra's stub.
	cmd := &cobra.Command{
		Use:                "session",
		Short:              "export/import identity + proxy state",
		Long:               "Export or import the targeted provider's identity and proxy state as a passphrase-encrypted bundle. 'save' prompts twice with echo off and writes the file owner-only; 'load' backs up the current identity first, refuses a bundle from a different account unless --allow-different-account is passed, and stages the new identity for the provider's next restart.",
		Example:            "  urnet-tools session save ~/urnet-session.enc           # Linux / macOS\n  urnet-tools session save C:\\Users\\<you>\\urnet-session.enc   # Windows (\\ or / separators)\n  urnet-tools session load ~/urnet-session.enc --unit urnetwork-native.service --force",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdSession(args)
		},
	}
	cmd.AddCommand(
		subPage("save", "save <file>", "export identity and proxy state as an encrypted bundle",
			"Write a passphrase-encrypted bundle of the targeted provider's identity and proxy state to <file>. The passphrase is prompted twice with echo off; the file is written owner-only.",
			"  urnet-tools session save ~/urnet-session.enc\n  urnet-tools session save C:\\Users\\<you>\\urnet-session.enc",
			cmdSession),
		withFlags(subPage("load", "load <file>", "import identity and proxy state from an encrypted bundle",
			"Import a session bundle into the targeted provider. The current identity is backed up first; a bundle from a different account is refused unless you pass --allow-different-account; the new identity is staged for the provider to pick up on restart.",
			"  urnet-tools session load ~/urnet-session.enc --unit urnetwork-native.service --force",
			cmdSession),
			helpFlag{Name: "allow-different-account", Usage: "accept a session bundle from a different account"}),
	)
	return withFlags(cmd,
		helpFlag{Name: "allow-different-account", Usage: "accept a session bundle from a different account"},
	)
}

func newTurboCmd() *cobra.Command {
	return withHelp(newCobraCmd("turbo", "RAISE throughput limits for RAM-rich boxes", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdTune("turbo", rest, force, dryRun)
		})
	}), "Show the current throughput profile, or set it to v4/v8 to raise limits on a RAM-rich box, or off to clear. Applied through the provider's control socket (queued when the provider is stopped) and followed by a provider restart, so it asks for a typed \"yes\" unless you pass -f/--force.", "  urnet-tools turbo v8\n  urnet-tools turbo off --unit urnetwork-native.service")
}

func newAutoCmd() *cobra.Command {
	return withHelp(newCobraCmd("auto", "AUTO-TUNE detect hardware and pick best profile", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdTune("auto", rest, force, dryRun)
		})
	}), "Show current auto-tune status, or turn it on/off to let the provider detect hardware and pick the best-fit profile. This sets the profile through the provider control socket (queued in pending_overrides.json if the provider is stopped) and restarts the provider unit, so it asks for a typed \"yes\" unless you pass -f/--force.", "  urnet-tools auto on\n  urnet-tools auto off --unit urnetwork-native.service")
}

func newEcoCmd() *cobra.Command {
	return withHelp(newCobraCmd("eco", "ECO MODE GC-tuned for low-RAM systems", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdTune("eco", rest, force, dryRun)
		})
	}), "Show current eco mode status, or turn it on/off (GC-tuned for low-RAM systems). This sets the profile through the provider control socket (queued in pending_overrides.json if the provider is stopped) and restarts the provider unit, so it asks for a typed \"yes\" unless you pass -f/--force.", "  urnet-tools eco on\n  urnet-tools eco off --user urnet")
}

func newLowmodeCmd() *cobra.Command {
	return withHelp(newCobraCmd("lowmode", "LOW-MEMORY reduced buffers for max RAM savings", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdTune("lowmode", rest, force, dryRun)
		})
	}), "Show current low-memory status, or turn it on/off (reduces buffers to save RAM). This sets the profile through the provider control socket (queued in pending_overrides.json if the provider is stopped) and restarts the provider unit, so it asks for a typed \"yes\" unless you pass -f/--force.", "  urnet-tools lowmode on\n  urnet-tools lowmode off --unit urnetwork-native.service")
}

func newRamlogsCmd() *cobra.Command {
	return withHelp(newCobraCmd("ramlogs", "RAM LOGS zero disk I/O logging", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdTune("ramlogs", rest, force, dryRun)
		})
	}), "Show current RAM logging status, or turn it on/off. RAM logging writes provider logs to a RAM buffer instead of disk. This writes a systemd drop-in and restarts the provider unit, so it asks for a typed \\\"yes\\\" unless you pass -f/--force.", "  urnet-tools ramlogs\n  urnet-tools ramlogs on\n  urnet-tools ramlogs off --network mynetwork")
}

func newOptimizeCmd() *cobra.Command {
	return withHelp(newCobraCmd("optimize", "apply golden-fleet OS/kernel limits", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdOptimize(rest, force, dryRun)
		})
	}), "Apply the golden-fleet OS and kernel network limits to this host (host-wide, not per provider): socket buffers, file descriptor limit, ephemeral ports and TIME_WAIT on Linux, or the netsh and registry equivalents on Windows. It asks for a typed \"yes\" unless you pass -f/--force, then re-executes itself under sudo and applies both the live settings and the reboot-persisted file.", "  urnet-tools optimize\n  urnet-tools optimize --force")
}

func newHotswapCmd() *cobra.Command {
	return withHelp(newCobraCmd("hotswap", "swap the provider process with no service gap (proxies rebuild ~30s)", []string{"hot-swap"}, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdHotswap(rest, force, dryRun)
		})
	}), "Trigger an in-process HotSwap on a running provider without cycling the unit. The provider process is swapped without a gap in service, but proxy connections still rebuild over about 30 s.", "  urnet-tools hotswap --unit urnetwork-native.service\n  urnet-tools hotswap --force")
}

func newFastAuthCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "fast-auth",
		Short:              "manage the auth rate limiter",
		Long:               "Manage the marker file that bypasses the provider's auth rate limiter. on writes the marker, off removes it, and status (the default) reports the current state without changing anything. Mutating actions ask for a typed \"yes\" unless you pass -f/--force.",
		Example:            "  urnet-tools fast-auth status\n  urnet-tools fast-auth on --unit urnetwork-native.service\n  urnet-tools fast-auth off --force",
		Aliases:            []string{"fastauth"},
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				fmt.Fprint(os.Stderr, "urnet-tools fast-auth - manage the auth rate limiter bypass\n\nUsage: urnet-tools fast-auth <on|off|status> [target]\n\n  on     bypass the auth rate limiter (writes the marker)\n  off    re-enable the rate limiter\n  status show the current state (read-only)\n")
				return nil
			}
			return parseGlobal(args, func(force, dryRun bool, rest []string) error {
				return cmdFastAuth(rest, force, dryRun)
			})
		},
	}
}

func newGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "get [key] [target]",
		Short:              "show the current value of a runtime override",
		Long:               "Show the current value of one runtime override applied to the targeted provider. With no key it lists every active override. This is the read-only face of `set`: pass a single key only, never a value — use `set <key> <value>` to change anything. Run 'get help' to list the available keys.",
		Example:            "  urnet-tools get gomemlimit\n  urnet-tools get --unit urnetwork-native.service\n  urnet-tools show gomemlimit",
		Aliases:            []string{"show"},
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				printSetHelp()
				return nil
			}
			return parseGlobal(args, func(force, dryRun bool, rest []string) error {
				return cmdGet(rest)
			})
		},
	}
}

// cmdGet implements `urnet-tools get [key] [target]` — a strictly read-only
// view of the provider's runtime overrides. It reuses the set machinery's
// read forms (no key = list, one key = value) but rejects any value form so
// a stray `get gomemlimit 1G` can never silently write.
func cmdGet(args []string) error {
	_, rest, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return fmt.Errorf("get is read-only: pass a single key (e.g. `urnet-tools get gomemlimit`); to change a value use `urnet-tools set <key> <value>`")
	}
	return cmdSet(args, false, false)
}

func newSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "set",
		Short:              "runtime tuning override",
		Long:               "Read or write a runtime tuning override in the provider's state dir. Most keys apply on the provider's next tick with no restart; profile and ramlogs take effect on the next restart. No key lists every active override, a key alone shows its value, a key with a value sets it, and \"off\" clears it back to the startup default. Run 'set help' for the keys and their value domains.",
		Example:            "  urnet-tools set help\n  urnet-tools set report-interval 5m --unit urnetwork-native.service\n  urnet-tools set cleanup-scope off",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				printSetHelp()
				return nil
			}
			return parseGlobal(args, func(force, dryRun bool, rest []string) error {
				return cmdSet(rest, force, dryRun)
			})
		},
	}
}

func newRenameCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "rename <name>",
		Short:              "set dashboard display name",
		Long:               "Set the provider's display name reported to the dashboard. Equivalent to `urnet-tools set node-name <name>`. The change takes effect on the provider's next tick — no restart needed. Clears the override with `urnet-tools rename off` (reverts to hostname).",
		Example:            "  urnet-tools rename us-west-2\n  urnet-tools rename off\n  urnet-tools rename my-node-3 --unit urnetwork-native.service",
		Aliases:            []string{"set-node-name"},
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			return parseGlobal(args, func(force, dryRun bool, rest []string) error {
				if len(rest) == 0 || (rest[0] == "off" && len(rest) > 1) {
					return fmt.Errorf("rename requires a name argument (or 'off' to clear)")
				}
				if rest[0] == "off" {
					rest = []string{"node-name", "off"}
				} else {
					rest = []string{"node-name", rest[0]}
				}
				return cmdSet(rest, force, dryRun)
			})
		},
	}
}

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "auth",
		Short:              "authenticate",
		Long:               "Authenticate the targeted provider against the URnetwork API by delegating to the provider binary's own auth subcommand. Pass an auth code, or omit it to use the provider's stored identity. Existing credentials are only overwritten with the provider's own -f flag, which passes through untouched.",
		Example:            "  urnet-tools auth\n  urnet-tools auth ABCD1234 --unit urnetwork-native.service",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdAuth(args)
		},
	}
	return withFlags(cmd,
		helpFlag{Name: "force", Shorthand: "f", Usage: "replace existing credentials (passed through to the provider's auth)"},
	)
}

func newChooseNetworkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "choose-network",
		Short:              "set API/connect endpoints",
		Long:               "Set the API URL and connect URL the targeted provider uses, or clear that override with --reset to revert to the main network. This delegates to the provider binary's choose_network subcommand and streams its output.",
		Example:            "  urnet-tools choose-network https://api.example.com wss://connect.example.com\n  urnet-tools choose-network --reset --unit urnetwork-native.service",
		Aliases:            []string{"choose_network"},
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdChooseNetwork(args)
		},
	}
	return withFlags(cmd,
		helpFlag{Name: "reset", Usage: "clear the endpoint override and revert to the main network"},
	)
}

func newProxyCmd() *cobra.Command {
	proxyDispatch := func(force, dryRun bool, rest []string) error {
		return cmdProxy(rest, force, dryRun)
	}
	proxyCmd := &cobra.Command{
		Use:                "proxy <subcommand>",
		Short:              "manage the provider's proxies",
		Long:               "Manage the targeted provider's proxies: add from a file, paste raw text, clear or remove entries, refresh URL sources, and inspect health, traffic and client IDs. Run 'urnet-tools proxy <subcommand> --help' for a subcommand's own page. The proxy summary is the top-level 'urnet-tools summary' command, not a proxy subcommand.",
		Example:            "  urnet-tools proxy add ~/proxies.txt\n  urnet-tools proxy health --unit urnetwork-native.service\n  urnet-tools proxy trim 50",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 || hasHelpFlag(args) || (len(args) == 1 && args[0] == "help") {
				return cmd.Help()
			}
			return parseGlobal(args, proxyDispatch)
		},
	}
	batchFlags := []helpFlag{
		{Name: "all", Usage: "apply to every provider on the box"},
		{Name: "include", Value: "A,B", Usage: "only providers whose label, user, unit or network matches one of these"},
		{Name: "exclude", Value: "A,B", Usage: "skip providers whose label, user, unit or network matches one of these"},
	}
	proxyCmd.AddCommand(
		withFlags(subPageTS("add", "add <file> [target]", "add proxies from a proxy file",
			"Read a proxy file (host:port, or host:port:user:pass per line) and bulk-add its entries to the targeted provider. A file given with --file is the same as the positional path.",
			"  urnet-tools proxy add ~/proxies.txt\n  urnet-tools proxy add ~/proxies.txt --unit urnetwork-native.service",
			proxyDispatch),
			helpFlag{Name: "file", Value: "PATH", Usage: "proxy `PATH` to add (the same as the positional path)"},
			batchFlags[0], batchFlags[1], batchFlags[2]),
		withFlags(subPageTS("paste", "paste [target]", "paste raw proxies from stdin, a file, or a URL",
			"Ingest raw proxy text from stdin (pipe it in), from --file, or from --url, normalize the common formats, and add the entries to the targeted provider.",
			"  cat proxies.txt | urnet-tools proxy paste\n  urnet-tools proxy paste --file=~/proxies.txt\n  urnet-tools proxy paste --url=https://example.com/proxies.txt",
			proxyDispatch),
			helpFlag{Name: "file", Value: "PATH", Usage: "read the raw proxies from a file at `PATH`"},
			helpFlag{Name: "url", Value: "URL", Usage: "read the raw proxies from `URL`"}),
		withFlags(subPageTS("clear", "clear [target]", "remove every proxy",
			"Remove all proxies from the targeted provider, unconditionally. Use remove for selective removal.",
			"  urnet-tools proxy clear --unit urnetwork-native.service",
			proxyDispatch),
			batchFlags[0], batchFlags[1], batchFlags[2]),
		withFlags(subPageTS("remove", "remove [<proxy>...] [target]", "remove proxies by address, --match, or all",
			"Remove specific proxies by address, or every proxy matching --match. With neither addresses nor --match this removes all proxies on the target, the same as clear.",
			"  urnet-tools proxy remove 1.2.3.4:5555 --unit urnetwork-native.service\n  urnet-tools proxy remove --match=198.51 --unit urnetwork-native.service",
			proxyDispatch),
			helpFlag{Name: "match", Value: "PATTERN", Usage: "remove every proxy whose address contains the pattern"},
			batchFlags[0], batchFlags[1], batchFlags[2]),
		withFlags(subPageTS("refresh", "refresh [target]", "re-read URL proxy sources now",
			"Re-fetch the targeted provider's URL proxy sources and hot-reload the proxies without a restart.",
			"  urnet-tools proxy refresh --unit urnetwork-native.service",
			proxyDispatch),
			batchFlags[0], batchFlags[1], batchFlags[2]),
		withFlags(subPageTS("add-source", "add-source <url> [target]", "add a URL proxy source",
			"Add a URL proxy source to the targeted provider. The source is fetched now, cached, and refreshed on the provider's own schedule.",
			"  urnet-tools proxy add-source https://example.com/proxies.txt --unit urnetwork-native.service",
			proxyDispatch),
			batchFlags[0], batchFlags[1], batchFlags[2]),
		withFlags(subPageTS("remove-source", "remove-source <url> [target]", "remove a URL proxy source",
			"Remove a URL proxy source from the targeted provider, so it is no longer fetched or refreshed.",
			"  urnet-tools proxy remove-source https://example.com/proxies.txt --unit urnetwork-native.service",
			proxyDispatch),
			batchFlags[0], batchFlags[1], batchFlags[2]),
		subPageTS("health", "health [target]", "proxy health from the provider's state files",
			"Show the targeted provider's proxy health from its state files: status counts and the newest health events. Single target, so run it once per provider.",
			"  urnet-tools proxy health --unit urnetwork-native.service",
			proxyDispatch),
		subPageTS("traffic", "traffic [target]", "real-time bandwidth and client load",
			"Show the targeted provider's per-proxy bandwidth and client session load from its state files, with the newest entries first.",
			"  urnet-tools proxy traffic --unit urnetwork-native.service",
			proxyDispatch),
		subPageTS("ids", "ids [target]", "client ID per proxy from the JWT store",
			"Show the client_id each proxy is serving from the targeted provider's JWT store. Useful to match a proxy address to the account it serves.",
			"  urnet-tools proxy ids --unit urnetwork-native.service",
			proxyDispatch),
		withFlags(subPageTS("remove-dead", "remove-dead [target]", "prune dead, degraded and failing proxies",
			"Walk the targeted provider's dead, degraded and failing proxies and remove the ones you confirm. Pass -n/--dry-run first to preview the list, or -f/--force to skip the confirmations.",
			"  urnet-tools proxy remove-dead --unit urnetwork-native.service\n  urnet-tools proxy remove-dead -n",
			proxyDispatch),
			batchFlags[0], batchFlags[1], batchFlags[2]),
		withFlags(subPageTS("trim", "trim <N|off> [target]", "hold running proxies at N, shed the worst first",
			"Hold the targeted provider at N running proxies, shedding the worst-graded (F to A) first, or pass off to clear the cap and let every proxy run again.",
			"  urnet-tools proxy trim 50 --unit urnetwork-native.service\n  urnet-tools proxy trim off",
			proxyDispatch),
			batchFlags[0], batchFlags[1], batchFlags[2]),
		withFlags(subPageTS("audit", "audit [<action>] [target]", "inspect or toggle the proxy quality audit",
			"Manage the automated proxy quality audit: status (the default) shows what it has parked and why, on and off toggle it on the fly, and release <proxy> (or release --all) unparks proxies it proved bad.",
			"  urnet-tools proxy audit status\n  urnet-tools proxy audit on\n  urnet-tools proxy audit release 1.2.3.4:1080",
			proxyDispatch),
			batchFlags[0], batchFlags[1], batchFlags[2]),
	)
	return proxyCmd
}

func newReinstallCmd() *cobra.Command {
	return withHelp(newCobraCmd("reinstall", "reinstall provider", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdReinstall(rest, force, dryRun)
		})
	}), "Re-fetch the current release's provider binary to its existing path, ensure the unit, and restart it, using the same verified download-and-swap path as update. Asks for a typed \"yes\" unless you pass -f/--force.", "  urnet-tools reinstall --unit urnetwork-native.service\n  urnet-tools reinstall --force")
}

func newUninstallCmd() *cobra.Command {
	return withHelp(newCobraCmd("uninstall", "uninstall provider", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdUninstall(rest, force, dryRun)
		})
	}), "Remove the targeted provider: disable and stop its unit, remove its auto-update artifacts, and delete the binary and state directory. This is fully destructive, so it asks for a typed \"yes\" unless you pass -f/--force, and refuses to touch anything that does not look like a real install path.", "  urnet-tools uninstall --unit urnetwork-native.service\n  urnet-tools uninstall --force")
}

func newAutoUpdateCmd() *cobra.Command {
	return withHelp(newCobraCmd("auto-update", "manage auto-update schedule", []string{"autoupdate"}, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdAutoUpdate(rest, force, dryRun)
		})
	}), "Set the auto-update schedule for the targeted provider to daily, weekly, or monthly, or turn it off. Honors --dry-run, which prints the plan and changes nothing.", "  urnet-tools auto-update daily --unit urnetwork-native.service\n  urnet-tools auto-update off")
}

func newAutoStartCmd() *cobra.Command {
	return withHelp(newCobraCmd("auto-start", "toggle auto-start on login", []string{"autostart"}, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdAutoStart(rest, force, dryRun)
		})
	}), "Turn on or off whether the targeted provider's unit starts automatically on login. Honors --dry-run, which prints the plan and changes nothing.", "  urnet-tools auto-start on --unit urnetwork-native.service\n  urnet-tools auto-start off")
}

func newSelfHealCmd() *cobra.Command {
	// cmdSelfHeal has its own -h handling; building raw preserves it.
	return &cobra.Command{
		Use:                "self-heal",
		Short:              "self heal",
		Long:               "Toggle or report the provider's self-heal marker file, which enables its automatic proxy load-gate and cleanup. Run with on, off, or status (the default with no argument).",
		Example:            "  urnet-tools self-heal status\n  urnet-tools self-heal on\n  urnet-tools self-heal off",
		Aliases:            []string{"selfheal"},
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Route -h/--help here so the per-command page renders instead of
			// the top-level menu that cmdSelfHeal would print.
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			return cmdSelfHeal(args)
		},
	}
}

func newIPDetectCmd() *cobra.Command {
	// cmdIPDetect has its own -h handling; building raw preserves it.
	return &cobra.Command{
		Use:     "show-ip",
		Short:   "show the public IP on the dashboard label",
		Long:    "Toggle or report whether the provider appends its public IP (fetched via ip.me) to the dashboard identity label set by 'rename'. This controls the dashboard display only, not which address the provider serves on; see 'direct' for that. When off, only the node name is reported unless URNETWORK_PUBLIC_IP is set.",
		Example: "  urnet-tools show-ip status\n  urnet-tools show-ip off\n  urnet-tools show-ip on",
		// ip-detect/ipdetect named the mechanism rather than the effect, and
		// read as a diagnostic that would report the IP. Kept as aliases so
		// existing scripts and runbooks keep working.
		Aliases:            []string{"ip-detect", "ipdetect"},
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			return cmdIPDetect(args)
		},
	}
}

func newHistoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "history [limit] [--cursor <cursor>]",
		Short:              "show command audit trail",
		Long:               "Show this box's command audit trail: the urnet-tools commands run against a provider, newest last, with their outcome. Read-only: reads the provider's audit ring from its state dir; needs no root beyond the file's own permissions. --cursor continues a paged read from where the last page ended. Alias: audit.",
		Example:            "  urnet-tools history\n  urnet-tools history 50\n  urnet-tools history 50 --cursor <cursor>",
		Aliases:            []string{"audit"},
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			return cmdHistory(args)
		},
	}
	return withFlags(cmd,
		helpFlag{Name: "cursor", Value: "CURSOR", Usage: "continue a paged read where the last page ended"},
	)
}

func newMetricsCmd() *cobra.Command {
	metricsDispatch := func(force, dryRun bool, rest []string) error {
		return cmdMetrics(rest, dryRun)
	}
	metricsCmd := &cobra.Command{
		Use:                "metrics <subcommand>",
		Short:              "show, toggle, or move the Prometheus /metrics endpoint",
		Long:               "Show, enable, disable, or move the provider's Prometheus /metrics endpoint. 'listen <ip:port|auto>' moves where it serves; 'auto' picks the listen address. Changes are applied to the running provider at its next tick, with no restart. Honors -n/--dry-run, which prints the plan and sends nothing.",
		Example:            "  urnet-tools metrics\n  urnet-tools metrics on\n  urnet-tools metrics listen 0.0.0.0:9099",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			// M4: command used to force dryRun=false unconditionally, so
			// `metrics on -n` applied the change. Route through the global
			// flag parser so -n/--dry-run (and -f) are honored.
			return parseGlobal(args, metricsDispatch)
		},
	}
	metricsCmd.AddCommand(
		subPageTS("status", "status", "show whether /metrics is on and where",
			"Show whether the provider's Prometheus /metrics endpoint is on, and the address to scrape when it is.",
			"  urnet-tools metrics status --unit urnetwork-native.service",
			metricsDispatch),
		subPageTS("on", "on", "start the /metrics listener",
			"Start the /metrics listener in the running provider and print the scrape address. Applied at the provider's next tick, with no restart.",
			"  urnet-tools metrics on --unit urnetwork-native.service",
			metricsDispatch),
		subPageTS("off", "off", "stop the /metrics listener",
			"Stop the /metrics listener in the running provider. Applied at the provider's next tick, with no restart.",
			"  urnet-tools metrics off --unit urnetwork-native.service",
			metricsDispatch),
		subPageTS("listen", "listen <ip:port|auto>", "choose the /metrics listen address",
			"Move the /metrics listener to a fixed ip:port, or pass auto to let the provider pick the address. Applied at the next tick and persisted.",
			"  urnet-tools metrics listen 0.0.0.0:9099\n  urnet-tools metrics listen auto",
			metricsDispatch),
	)
	return metricsCmd
}

// isTruthy returns true for any string representation that a human would
// consider "enabled": on/1/true/yes and their case variants.
func isTruthy(s string) bool {
	switch strings.ToLower(s) {
	case "on", "1", "true", "yes":
		return true
	}
	return false
}

// parseHistoryArgs extracts --cursor, target flags, and an optional
// positional limit from the raw args passed to `history`.
// It returns the limit (default 50, capped at 100), the cursor string,
// and the parsed target. Flags are parsed in the correct order so that
// `history --unit X`, `history 30 --cursor C --unit X`, etc. all work.
func parseHistoryArgs(args []string) (limit int, cursor string, t Target, err error) {
	limit = 50

	// First pass: extract --cursor before parseTargetFlags (which
	// rejects unknown --flags in strict mode).  Handle both
	// `--cursor X` and `--cursor=X` forms.
	var filtered []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if v, ok := strings.CutPrefix(a, "--cursor="); ok {
			if v == "" {
				return 0, "", t, fmt.Errorf("--cursor requires a value")
			}
			cursor = v
			continue
		}
		if a == "--cursor" {
			if i+1 >= len(args) {
				return 0, "", t, fmt.Errorf("--cursor requires a value")
			}
			cursor = args[i+1]
			i++
			continue
		}
		filtered = append(filtered, a)
	}

	// Second pass: extract target flags and the positional limit.
	t, rest, err := parseTargetFlags(filtered)
	if err != nil {
		return 0, "", t, err
	}

	if len(rest) > 0 {
		n, parseErr := strconv.Atoi(rest[0])
		if parseErr != nil || n <= 0 {
			return 0, "", t, fmt.Errorf("limit must be a positive integer (got %q)", rest[0])
		}
		if n > 100 {
			n = 100
		}
		limit = n
	}
	return limit, cursor, t, nil
}

func cmdHistory(args []string) error {
	limit, cursor, t, err := parseHistoryArgs(args)
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
	resp, err := sendSocketRequest(socketPath, controlRequest{Cmd: "history", Limit: limit, Cursor: cursor})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("provider returned error: %s", resp.Error)
	}

	if len(resp.Entries) == 0 {
		fmt.Println("No audit entries.")
		return nil
	}

	fmt.Printf("Last %d audit entries:\n\n", len(resp.Entries))
	for _, e := range resp.Entries {
		ts, parseErr := time.Parse(time.RFC3339, e.Timestamp)
		tsStr := e.Timestamp
		if parseErr == nil {
			tsStr = ts.Local().Format("2006-01-02 15:04:05")
		}
		if e.OK {
			fmt.Printf("[%s] OK  %s %s=%s\n", tsStr, e.Cmd, e.Key, e.Value)
		} else {
			fmt.Printf("[%s] ERR %s %s=%s  error: %s\n", tsStr, e.Cmd, e.Key, e.Value, e.Error)
		}
	}
	if resp.NextCursor != "" {
		fmt.Printf("\nMore entries available. Use 'urnet-tools history --cursor %s' for older entries.\n", resp.NextCursor)
	}
	return nil
}

// sendMetricsToggle asks the provider to set the metrics key over its
// control socket. Split out of cmdMetrics so the socket path it dials is
// reachable from a test without going through target discovery: the path
// was wrong for the whole life of this command and no test could see it.
func sendMetricsToggle(p Provider, val string) (controlResponse, error) {
	socketPath := filepath.Join(p.StateDir, "provider.sock")
	return sendSocketRequest(socketPath, controlRequest{Cmd: "set", Key: "metrics", Value: val})
}

func cmdMetrics(args []string, dryRun bool) error {
	t, rest, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	const usage = "usage: urnet-tools metrics [status|on|off|listen <ip:port|auto>]"
	action := "status"
	if len(rest) > 0 {
		action = strings.ToLower(rest[0])
	}
	var change *controlRequest
	switch action {
	case "status":
	case "on", "off":
		change = &controlRequest{Cmd: "set", Key: "metrics", Value: action}
	case "listen":
		if len(rest) < 2 {
			return fmt.Errorf("usage: urnet-tools metrics listen <ip:port|auto>")
		}
		if err := validateControlValue("metrics_listen", rest[1]); err != nil {
			return err
		}
		if strings.EqualFold(rest[1], "auto") || strings.EqualFold(rest[1], "off") {
			change = &controlRequest{Cmd: "clear", Key: "metrics_listen"}
		} else {
			change = &controlRequest{Cmd: "set", Key: "metrics_listen", Value: rest[1]}
		}
	default:
		return fmt.Errorf("%s (got %q)", usage, rest[0])
	}

	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	socketPath := filepath.Join(p.StateDir, "provider.sock")

	if change != nil {
		if dryRun {
			fmt.Printf("[dry-run] would send %s %s %s to %s\n", change.Cmd, change.Key, change.Value, providerLabel(p))
			return nil
		}
		var resp controlResponse
		if change.Key == "metrics" {
			resp, err = sendMetricsToggle(p, change.Value)
		} else {
			resp, err = sendSocketRequest(socketPath, *change)
		}
		if err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("provider returned error: %s", resp.Error)
		}
	}

	status, err := sendSocketRequest(socketPath, controlRequest{Cmd: "status"})
	if err != nil {
		return err
	}
	if !status.OK {
		return fmt.Errorf("provider returned error: %s", status.Error)
	}
	printMetricsStatus(os.Stdout, status)
	return nil
}

// ---------------------------------------------------------------------------
// urnet-tools smart-dialer — measured-cost transport preference
// ---------------------------------------------------------------------------

func newSmartDialerCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "smart-dialer [status|on|off]",
		Short:              "show or toggle measured-cost transport preference (skip DPI-circumvention cost when the network does not need it)",
		Long:               "Show or toggle the measured-cost transport preference: when the network does not need DPI circumvention, the provider prefers transport paths that avoid its cost. 'status' (the default) reports the current value; on and off change it. Applied to the running provider at its next tick, with no restart. Honors -n/--dry-run. Alias: smart_dialer.",
		Example:            "  urnet-tools smart-dialer\n  urnet-tools smart-dialer on\n  urnet-tools smart-dialer off --unit urnetwork-native.service",
		Aliases:            []string{"smart_dialer"},
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			// Route through the global flag parser so -n/--dry-run (and -f)
			// are honored: a dry-run must report the planned change without
			// sending the set request to the provider.
			return parseGlobal(args, func(force, dryRun bool, rest []string) error {
				return cmdSmartDialer(rest, dryRun)
			})
		},
	}
}

func cmdSmartDialer(args []string, dryRun bool) error {
	t, rest, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	const usage = "usage: urnet-tools smart-dialer [status|on|off]"
	if len(rest) > 1 {
		return fmt.Errorf("%s (got %q)", usage, strings.Join(rest, " "))
	}
	action := "status"
	if len(rest) > 0 {
		action = strings.ToLower(rest[0])
	}
	var change *controlRequest
	switch action {
	case "status":
	case "on", "off":
		change = &controlRequest{Cmd: "set", Key: "smart_dialer", Value: action}
	default:
		return fmt.Errorf("%s (got %q)", usage, rest[0])
	}

	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	socketPath := filepath.Join(p.StateDir, "provider.sock")

	if change != nil {
		if err := validateControlValue(change.Key, change.Value); err != nil {
			return err
		}
		if dryRun {
			fmt.Printf("[dry-run] would send %s %s %s to %s\n", change.Cmd, change.Key, change.Value, providerLabel(p))
			return nil
		}
		resp, err := sendSocketRequest(socketPath, *change)
		if err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("provider returned error: %s", resp.Error)
		}
	}

	status, err := sendSocketRequest(socketPath, controlRequest{Cmd: "status"})
	if err != nil {
		return err
	}
	if !status.OK {
		return fmt.Errorf("provider returned error: %s", status.Error)
	}
	printSmartDialerStatus(os.Stdout, status)
	return nil
}

func printSmartDialerStatus(w io.Writer, resp controlResponse) {
	setting, ok := resp.Settings["smart_dialer"]
	if ok && isTruthy(setting.Value) {
		fmt.Fprintln(w, "Smart dialer: on")
		fmt.Fprintln(w, "  The provider prefers the transport that measures fastest on this network.")
		fmt.Fprintln(w, "  A transport that stops working here is avoided automatically, so this stays")
		fmt.Fprintln(w, "  safe on networks that need DPI circumvention.")
		fmt.Fprintln(w, "Turn off with: urnet-tools smart-dialer off")
		return
	}
	fmt.Fprintln(w, "Smart dialer: off")
	fmt.Fprintln(w, "  Transport preference uses the fixed baseline scoring, which costs time on")
	fmt.Fprintln(w, "  networks that do not need DPI circumvention.")
	fmt.Fprintln(w, "Turn on with: urnet-tools smart-dialer on")
}

// ---------------------------------------------------------------------------
// urnet-tools profile — show or set the memory/GC tuning profile
// ---------------------------------------------------------------------------

var profileDescriptions = map[string]string{
	"auto":     "auto-detect hardware and pick the best profile",
	"turbo-v4": "high throughput, 4-core optimized (large buffers, aggressive GC)",
	"turbo-v8": "high throughput, 8-core optimized (largest buffers)",
	"eco":      "low RAM, GC-tuned for memory-constrained systems",
	"lowmem":   "minimal buffers, shared-memory logs, maximum memory savings",
}

func newProfileCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "profile [auto|turbo-v4|turbo-v8|eco|lowmem]",
		Short:              "show or set the memory/GC tuning profile",
		Long:               "Show or set the provider's memory and GC tuning profile: auto sizes the box, turbo-v4 and turbo-v8 raise throughput on RAM-rich machines, eco tunes GC for low RAM, lowmem shrinks buffers for maximum savings. With no argument it prints the current value. Changes take effect on the provider's next restart; turbo, eco and lowmode are the runtime equivalents. Alias: profiles.",
		Example:            "  urnet-tools profile\n  urnet-tools profile auto\n  urnet-tools profile lowmem --unit urnetwork-native.service",
		Aliases:            []string{"profiles"},
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			return cmdProfile(args)
		},
	}
}

func cmdProfile(args []string) error {
	t, rest, err := parseTargetFlags(args)
	if err != nil {
		return err
	}

	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}

	// No args or just target flags: show current profile
	if len(rest) == 0 {
		return showProfile(p)
	}

	// Show profiles list
	if rest[0] == "list" || rest[0] == "ls" {
		return listProfiles(p)
	}

	// Set profile
	profile := rest[0]
	valid := false
	for name := range profileDescriptions {
		if profile == name {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("unknown profile %q — valid: auto, turbo-v4, turbo-v8, eco, lowmem", profile)
	}

	// Check if provider is running — profile is a startup-time setting
	if !p.Running {
		// Queue for next start
		if err := validateControlValue("profile", profile); err != nil {
			return err
		}
		if err := queuePendingOverrideIn(p.StateHome, p.StateDir, "set", "profile", profile); err != nil {
			return fmt.Errorf("queue pending override: %w", err)
		}
		fmt.Printf("Profile set to %s for %s (queued — takes effect on next start)\n", profile, providerLabel(p))
		return nil
	}

	// Provider is running — send via socket (will need restart)
	_, needsRestart, err := applyControlOverride(p, "set", "profile", profile, false)
	if err != nil {
		return err
	}
	fmt.Printf("Profile set to %s for %s\n", profile, providerLabel(p))
	if needsRestart {
		fmt.Printf("  ⚠ profile requires a restart to take effect\n")
		fmt.Printf("    systemctl --user restart %s\n", p.Unit)
	}
	return nil
}

func showProfile(p Provider) error {
	// Query current profile from socket or pending overrides
	val, source, found, err := queryControlOverride(p, "profile")
	if err != nil {
		return err
	}
	if !found {
		val = "auto (default)"
		source = "startup"
	}

	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintf(w, "profile:	%s\n", val)
	fmt.Fprintf(w, "source:	%s\n", source)
	fmt.Fprintf(w, "running:	%v\n", p.Running)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Available profiles:")
	for name, desc := range profileDescriptions {
		marker := "  "
		if val == name {
			marker = "→ "
		}
		fmt.Fprintf(w, "  %s%-12s %s\n", marker, name, desc)
	}
	return w.Flush()
}

func listProfiles(p Provider) error {
	val, _, found, _ := queryControlOverride(p, "profile")
	if !found {
		val = "auto"
	}

	fmt.Println("Available profiles:")
	fmt.Println()
	for name, desc := range profileDescriptions {
		marker := "  "
		if val == name {
			marker = "→ "
		}
		fmt.Printf("  %s%-12s %s\n", marker, name, desc)
	}
	fmt.Println()
	fmt.Printf("Current: %s\n", val)
	return nil
}

// ---------------------------------------------------------------------------
// urnet-tools dashboard — rich TUI status panel
// ---------------------------------------------------------------------------

const (
	colorReset  = "\033[0m"
	colorBold   = "\033[1m"
	colorDim    = "\033[2m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorRed    = "\033[31m"
	colorCyan   = "\033[36m"
	colorWhite  = "\033[97m"
)

func newDashboardCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "dashboard [target]",
		Short:              "rich provider status dashboard",
		Long:               "A full-screen, read-only status view of one provider: identity and network, systemd state and uptime, proxy counts and health, bandwidth and client sessions, and the recent capacity decisions. Nothing changes: it reads the provider's state and control socket and prints the panel. Aliases: dash, panel.",
		Example:            "  urnet-tools dashboard\n  urnet-tools dashboard --unit urnetwork-native.service",
		Aliases:            []string{"dash", "panel"},
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			return cmdDashboard(args)
		},
	}
}

func cmdDashboard(args []string) error {
	t, _, err := parseTargetFlags(args)
	if err != nil {
		return err
	}

	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}

	bold := colorBold
	dim := colorDim
	green := colorGreen
	yellow := colorYellow
	red := colorRed
	cyan := colorCyan
	reset := colorReset

	// Header
	fmt.Println()
	fmt.Printf("%s%s╔══════════════════════════════════════════════════════════╗%s\n", bold, cyan, reset)
	fmt.Printf("%s%s║  URNetwork Provider Dashboard                          ║%s\n", bold, cyan, reset)
	fmt.Printf("%s%s╚══════════════════════════════════════════════════════════╝%s\n", bold, cyan, reset)
	fmt.Println()

	// Status line
	state := "STOPPED"
	stateColor := red
	if p.Running {
		state = "RUNNING"
		stateColor = green
	}
	fmt.Printf("  %sState:%s   %s%s%s\n", bold, reset, stateColor, state, reset)

	// Version + uptime
	fmt.Printf("  %sVersion:%s %s\n", bold, reset, p.Version)
	if p.PID > 0 {
		fmt.Printf("  %sPID:%s     %d\n", bold, reset, p.PID)
	}

	// Network identity — a provider with no readable JWT (ghost/discovered-bare
	// record, stopped unit, empty identity) has an empty NetworkID; slice it
	// defensively instead of panicking on ""[:8].
	netID := p.NetworkID
	if len(netID) > 8 {
		netID = netID[:8] + "..."
	}
	if netID == "" {
		netID = "(none)"
	}
	fmt.Printf("  %sNetwork:%s %s (%s)\n", bold, reset, p.netLabel(), netID)

	// JWT expiry
	if !p.JWTExpires.IsZero() {
		remaining := time.Until(p.JWTExpires)
		expiryColor := green
		if remaining < 24*time.Hour {
			expiryColor = yellow
		}
		if remaining < 0 {
			expiryColor = red
		}
		fmt.Printf("  %sJWT:%s     %s%sexpires in %s%s\n", bold, reset, expiryColor, dim, remaining.Round(time.Minute), reset)
	}

	// Control socket
	socketOK := controlSocketReachable(p)
	socketColor := green
	socketText := "reachable"
	if !socketOK {
		socketColor = red
		socketText = "unreachable"
	}
	fmt.Printf("  %sSocket:%s  %s%s%s\n", bold, reset, socketColor, socketText, reset)

	// Profile
	val, _, found, _ := queryControlOverride(p, "profile")
	profileName := "auto"
	if found && val != "" {
		profileName = val
	}
	fmt.Printf("  %sProfile:%s %s%s%s\n", bold, reset, cyan, profileName, reset)

	fmt.Println()
	fmt.Printf("  %s── Active Settings ──%s\n", dim, reset)
	fmt.Println()

	// Show all active overrides
	overrides := []struct{ key, desc string }{
		{"fast_auth", "Fast Auth"},
		{"proxy_self_heal", "Proxy Self-Heal"},
		{"hot_restart", "Hot Restart"},
		{"report_url", "Report URL"},
		{"report_interval", "Report Interval"},
		{"proxy_url_max", "Max Proxies"},
		{"proxy_url_refresh", "URL Refresh"},
		{"proxy_dead_cleanup_scope", "Dead Cleanup Scope"},
		{"proxy_dead_cleanup_interval", "Dead Cleanup Interval"},
		{"node_name", "Node Name"},
		{"gomemlimit", "Go Memory Limit"},
		{"gogc", "Go GC Percent"},
		{"metrics", "Prometheus Metrics"},
	}

	for _, o := range overrides {
		val, _, found, _ := queryControlOverride(p, o.key)
		if found && val != "" {
			fmt.Printf("  %s%-24s%s %s%s%s\n", bold, o.key+":", reset, yellow, val, reset)
		}
	}

	// Pending restart indicators — query the provider's status once to
	// compare current values against what the process started with.
	var changedKeys []string
	var startupValues map[string]string
	if p.Running {
		sockPath := filepath.Join(p.StateDir, "provider.sock")
		if resp, err := sendSocketRequest(sockPath, controlRequest{Cmd: "status"}); err == nil && resp.OK {
			startupValues = resp.StartupValues
		}
		// Keys whose values are boolean-like but may be stored in different
		// representations ("on" vs "1", "true" vs "yes") across the control
		// state, pending overrides, and startup env vars.
		boolKeys := map[string]bool{"ramlogs": true}

		restartKeyList := []string{"profile", "ramlogs"}
		for _, key := range restartKeyList {
			curVal, _, found, _ := queryControlOverride(p, key)
			if !found {
				// Key was cleared (e.g. "set profile off") but the
				// running process may still hold the old value.
				if startupValues != nil && startupValues[key] != "" {
					changedKeys = append(changedKeys, key)
				}
				continue
			}
			if startupValues == nil {
				// Old provider without StartupValues — fall back
				// to legacy heuristic (warn if non-default).
				if curVal != "" && curVal != "off" && curVal != "0" {
					changedKeys = append(changedKeys, key)
				}
			} else if boolKeys[key] {
				// Boolean-like keys: normalize both sides so
				// "on" vs "1" and "off" vs "0" match correctly.
				if isTruthy(curVal) != isTruthy(startupValues[key]) {
					changedKeys = append(changedKeys, key)
				}
			} else if curVal != startupValues[key] {
				changedKeys = append(changedKeys, key)
			}
		}
	}
	if len(changedKeys) > 0 {
		fmt.Println()
		for _, key := range changedKeys {
			val, _, found, _ := queryControlOverride(p, key)
			if found && val != "" && val != "off" && val != "0" {
				fmt.Printf("  %s⚠ %s requires restart (%s)%s\n", yellow, key, val, reset)
			}
		}
	}

	fmt.Println()
	fmt.Printf("  %s── Proxy Sources ──%s\n", dim, reset)
	fmt.Println()

	// Read proxy_url.json for URL sources (must match the provider's
	// ProxyURLState object shape, not an array).
	if p.StateDir != "" {
		sources := readProxyURLSources(p.StateDir)
		if len(sources) > 0 {
			for _, src := range sources {
				fmt.Printf("  %s•%s %s\n", green, reset, src)
			}
		} else {
			fmt.Printf("  %s(no proxy sources configured)%s\n", dim, reset)
		}
	}

	fmt.Println()
	fmt.Printf("  %s── Quick Actions ──%s\n", dim, reset)
	fmt.Println()
	fmt.Printf("  urnet-tools set <key> <value>    Change a setting\n")
	fmt.Printf("  urnet-tools profile <name>       Switch tuning profile\n")
	fmt.Printf("  urnet-tools metrics [on|off|listen] Prometheus metrics status and control\n")
	fmt.Printf("  urnet-tools history              View command audit trail\n")
	if p.Running && len(changedKeys) > 0 {
		fmt.Printf("\n  %s⚠ Restart pending: systemctl --user restart %s%s\n", yellow, p.Unit, reset)
	}
	fmt.Println()

	return nil
}
