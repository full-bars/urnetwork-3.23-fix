package urnettools

import (
	"encoding/json"
	"fmt"
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
  hotswap                 Zero-downtime in-process binary reload
  self-update             Update this tool binary itself
  status                  Show provider service status
  logs [all|dump|-i]      Stream logs (all=from start, dump=save, -i=important only)

Performance & Tuning:
  turbo <v4|v8|off>       RAISE throughput limits for RAM-rich boxes
  auto <on|off>           AUTO-TUNE detect hardware and pick best profile
  eco <on|off>            ECO MODE GC-tuned for low-RAM systems
  lowmode <on|off>        LOW-MEMORY reduced buffers for max RAM savings
  ramlogs <on|off>        RAM LOGS zero disk I/O logging
  hot-restart <on|off>    restart provider (hot-restart is a config toggle)
  optimize                Apply Golden Fleet OS/kernel limits
  set [<k> [<v>|off]]     Show or change runtime tuning overrides
  fast-auth [on|off]      Bypass auth rate limiter without restart
  config [--json]         Show all provider settings with source and age

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
  proxy refresh [--force] Re-read configs and hot-reload proxies
  proxy trim <N>          Hold running proxies at N, shed worst first
  proxy health            Show dead/degraded proxies + live event log
  proxy traffic           Real-time bandwidth & client session load
  proxy ids               Client IDs for each proxy (from JWT store)
  proxy summary           Fleet-style summary (sources, health, counts)
  proxy remove-dead       Prune dead/degraded/failing proxies interactively
  report [<url>|off]      Set hub report URL
  self-heal [on|off]      Auto-regulate proxies (load gate + cleanup)
  show-ip [on|off|status] Show public IP on the dashboard label (was: ip-detect)
  direct [on|off]         Toggle providing on the machine's direct/local IP
  usage [graph[s] <view>] Traffic accounting: billable vs control, time-series

Hub Management:
  hub init                        Initialize hub and generate CA certificate
  hub link <url> [--token]        Fetch CA cert and pin the hub identity
  hub unlink                      Revert to HTTP (remove pin + CA cert)
  hub test [<url>]                Probe TLS connection, verify cert
  hub set <host:port>             Set legacy HTTP hub report URL
  hub off                         Stop reporting to hub (no restart)
  hub onboard-cmd                 Mint 15-min join token, print curl/sh line
  hub show-password               Show CA password (printed once after init)
  hub open-port <port>            Open port in firewall
  hub install [--docker]          Install hub as service
  hub update [--docker]           Update hub to latest version

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
	// The old dispatcher had no 'completion' subcommand; keep the surface stable.
	rootCmd.CompletionOptions.DisableDefaultCmd = true

	rootCmd.PersistentFlags().String("unit", "", "systemd unit, e.g. urnetwork-native.service")
	rootCmd.PersistentFlags().String("user", "", "OS user, e.g. urnet")
	rootCmd.PersistentFlags().String("network", "", "JWT network name, e.g. tacogonzalez3000")
	rootCmd.PersistentFlags().String("network-id", "", "JWT network id")
	rootCmd.PersistentFlags().String("state-dir", "", "state dir")
	rootCmd.PersistentFlags().BoolP("force", "f", false, "skip confirm prompts ONLY")
	rootCmd.PersistentFlags().BoolP("dry-run", "n", false, "print the plan, change nothing")

	rootCmd.AddCommand(
		newProvidersCmd(),
		newStatusCmd(),
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
		newHotRestartCmd(),
		newHotswapCmd(),
		newFastAuthCmd(),
		newSetCmd(),
		newAuthCmd(),
		newChooseNetworkCmd(),
		newProxyCmd(),
		newDirectCmd(),
		newUsageCmd(),
		newReportCmd(),
		newHubCmd(),
		newReinstallCmd(),
		newUninstallCmd(),
		newAutoUpdateCmd(),
		newAutoStartCmd(),
		newSelfHealCmd(),
		newDoRestartCmd(), // HIDDEN internal entry point for the updater's escalated restart
		newIPDetectCmd(),
		newRenameCmd(),
		newHistoryCmd(),
		newMetricsCmd(),
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

	return rootCmd
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

func newProvidersCmd() *cobra.Command {
	return withHelp(newCobraCmd("providers [--all]", "list providers on this box", []string{"list", "ps"}, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdProviders(rest)
		})
	}), "List providers found on this box: systemd units and bare processes, identified by their JWT network identity. By default an unprivileged caller sees only the providers owned by their own OS user (the one-provider-per-user contract); pass --all (run as root to read every identity) to list all providers across users. If no systemd providers exist but provider containers do, it says so and points you at urnet-docker.", "  urnet-tools providers\n  urnet-tools providers --all")
}

func newStatusCmd() *cobra.Command {
	return withHelp(newCobraCmd("status [target]", "detailed status of one provider", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdStatus(rest)
		})
	}), "Show detailed status for one provider: user, unit, binary, version, state dir, PID, running state, network identity, and JWT expiry. On Linux this reproduces `systemctl status <unit>`; on Windows and macOS it renders a status panel with a proxy summary. Target a specific provider with --unit, --user, --network, --network-id, or --state-dir.", "  urnet-tools status\n  urnet-tools status --network tacogonzalez3000\n  urnet-tools status --unit urnetwork-native.service")
}

func newSnStatusCmd() *cobra.Command {
	return withHelp(newCobraCmd("sn-status [target]", "Subnet 25 node rank, coldkey & miner status", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdSnStatus(rest)
		})
	}), "Show real-time Subnet 25 metrics: global network rank, billable bandwidth, registered Bittensor coldkey, Top 200 cutoff eligibility, contract clock, and payout share. Supports --json for machine-readable automation. Target a specific provider with --unit, --user, --network, --network-id, or --state-dir.", "  urnet-tools sn-status\n  urnet-tools sn-status --json\n  urnet-tools sn-status --unit urnetwork-native.service")
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
	}), "Restart the provider's systemd unit. This is a production action, so it asks for a typed \"yes\" unless you pass -f/--force (or -y/--yes). Use -n/--dry-run to print the plan without acting.", "  urnet-tools restart --unit urnetwork-native.service\n  urnet-tools restart --network tacogonzalez3000 --force")
}

func newUpdateCmd() *cobra.Command {
	return withHelp(newCobraCmd("update [target]", "update provider(s) to latest", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdUpdate(rest, force, dryRun)
		})
	}), "Download and install the latest provider release, verify its sha256 digest, swap the binary, and restart the owning unit. With no target and multiple providers it prompts interactively; use --all to update every provider, or --include/--exclude to pick a subset. Pin a release with --tag, or an exact asset with --digest and --url. This also refreshes the urnet-tools binary itself from the same release.", "  urnet-tools update\n  urnet-tools update --unit urnetwork-native.service\n  urnet-tools update --all --force\n  urnet-tools update --tag v3.23.0-fix.30.5")
}

func newIdleUpdateCmd() *cobra.Command {
	long := `Wait for billable traffic to drop below a threshold before applying a provider update.

Instead of updating immediately and severing active client connections mid-flight,
idle-update continuously polls the provider's billable relay traffic rate. When traffic
remains below --threshold for a sustained --window, it performs a 5-second verification
pass and then downloads, verifies, and installs the update.

A fallback --timeout ceiling ensures maintenance runs and automated scripts do not wait
indefinitely if low-volume background traffic never fully ceases. When the timeout is
reached, it logs a notice and proceeds with the update.

Flags:
  --threshold <bytes/s>   Max billable throughput to be considered idle (default: 5120 = 5 KiB/s)
  --window <duration>     Duration traffic must stay quiet (default: 5m; e.g. 300, 5m, 10m; 0 = immediate)
  --timeout <duration>    Maximum wait time before forcing the update (default: 30m; e.g. 1800, 30m, 1h; 0 = infinite)
  --tag <version>         Pin to a specific release tag (e.g. v3.23.0-fix.30.9)
  -f, --force             Skip interactive confirmation prompts
  -n, --dry-run           Print the traffic wait plan and release target without modifying anything`

	examples := `  # Wait for up to 30m for a 5-minute quiet window below 5 KiB/s:
  urnet-tools idle-update

  # Target a specific systemd unit with custom 10 KiB/s threshold and 1-hour timeout:
  urnet-tools idle-update --unit urnetwork-native.service --threshold 10240 --window 10m --timeout 1h

  # Immediate update bypass (equivalent to 'update' but through idle-update interface):
  urnet-tools idle-update --window 0

  # Preview the wait parameters and target release without acting:
  urnet-tools idle-update --dry-run`

	return withHelp(newCobraCmd("idle-update [target]", "wait for traffic lull before updating provider(s)", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdIdleUpdate(rest, force, dryRun)
		})
	}), long, examples)
}

func newSelfUpdateCmd() *cobra.Command {
	return withHelp(newCobraCmd("self-update", "update this tool binary itself", []string{"selfupdate"}, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdSelfUpdate(rest, force, dryRun)
		})
	}), "Update only the urnet-tools binary itself to the latest release, verifying its sha256 digest before swapping it in. No provider is touched or restarted. Pin a version with --tag, or point at an exact asset with --digest and --url.", "  urnet-tools self-update\n  urnet-tools self-update --tag v3.23.0-fix.30.5\n  urnet-tools self-update --force")
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
	}), "Show a fleet-style activity and performance summary for one provider. This delegates to the provider binary's own proxy summary command, so it needs the provider binary to be reachable and running.", "  urnet-tools summary\n  urnet-tools summary --network tacogonzalez3000")
}

func newVersionCmd() *cobra.Command {
	// '-v'/'--version' were dead aliases: Cobra strips '-' tokens before alias
	// matching, so they never resolve (handled at top level). Keep plain 'version'.
	return withHelp(newCobraCmd("version", "print this tool's version", nil, func(cmd *cobra.Command, args []string) error {
		fmt.Println(ToolVersion)
		return nil
	}), "Print the urnet-tools build version and exit. No provider is contacted.", "  urnet-tools version")
}

func newDefaultCmd() *cobra.Command {
	return withHelp(newCobraCmd("default", "persist a default provider target for this box", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDefault(args)
	}), "Persist, show, or clear a default provider target for this box, so later commands with no explicit target resolve to it on multi-provider boxes. 'default set' requires exactly one of --unit, --user, --network, --network-id, or --state-dir; the default is stored per user and is ignored automatically if it becomes stale or ambiguous.", "  urnet-tools default set --network tacogonzalez3000\n  urnet-tools default show\n  urnet-tools default clear")
}

func newSessionCmd() *cobra.Command {
	// cmdSession owns its rich help (save|load <file>, --allow-different-account);
	// building raw here lets that help fire instead of Cobra's stub.
	return &cobra.Command{
		Use:                "session",
		Short:              "export/import identity + proxy state",
		Long:               "Export or import the targeted provider's identity and proxy state as a passphrase-encrypted bundle. 'session save' prompts twice for a passphrase with echo off and writes the file with owner-only permissions. 'session load' backs up the current identity first, refuses a bundle from a different account unless you pass --allow-different-account, and stages the new identity for the provider to pick up on restart.",
		Example:            "  urnet-tools session save ~/urnet-session.enc           # Linux / macOS\n  urnet-tools session save C:\\Users\\<you>\\urnet-session.enc   # Windows (\\ or / separators)\n  urnet-tools session load ~/urnet-session.enc --unit urnetwork-native.service --force",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdSession(args)
		},
	}
}

func newTurboCmd() *cobra.Command {
	return withHelp(newCobraCmd("turbo", "RAISE throughput limits for RAM-rich boxes", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdTune("turbo", rest, force, dryRun)
		})
	}), "Show current throughput profile, or set it to v4/v8 to raise limits on a RAM-rich box (or off to clear). This sets the profile through the provider control socket (queued in pending_overrides.json if the provider is stopped) and restarts the provider unit, so it asks for a typed \"yes\" unless you pass -f/--force. Target a specific provider with --unit, --user, --network, or --network-id.", "  urnet-tools turbo v8\n  urnet-tools turbo off --unit urnetwork-native.service")
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
	}), "Show current RAM logging status, or turn it on/off. RAM logging writes provider logs to a RAM buffer instead of disk. This writes a systemd drop-in and restarts the provider unit, so it asks for a typed \\\"yes\\\" unless you pass -f/--force.", "  urnet-tools ramlogs\n  urnet-tools ramlogs on\n  urnet-tools ramlogs off --network tacogonzalez3000")
}

func newOptimizeCmd() *cobra.Command {
	return withHelp(newCobraCmd("optimize", "apply golden-fleet OS/kernel limits", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdOptimize(rest, force, dryRun)
		})
	}), "Apply golden-fleet OS and kernel network limits to this host: socket buffers, file descriptor limit, ephemeral port range, and TIME_WAIT timeout on Linux, or the netsh and registry equivalents on Windows. This is host-wide, not per provider, so no target flag applies. It asks for a typed \"yes\" unless you pass -f/--force (or -y/--yes), then prompts for sudo as needed on Linux and applies both the live settings and the reboot-persisted file. Run it as a normal user — it re-executes itself under sudo.", "  urnet-tools optimize\n  urnet-tools optimize --force")
}

func newHotRestartCmd() *cobra.Command {
	return withHelp(newCobraCmd("hot-restart", "restart provider (hot-restart is a config toggle)", []string{"hotrestart"}, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdHotRestart(rest, force, dryRun)
		})
	}), "Restart the provider's unit in a way that lets it reuse client IDs across the restart. It takes no extra arguments beyond a target, and asks for a typed \"yes\" unless you pass -f/--force.", "  urnet-tools hot-restart --unit urnetwork-native.service\n  urnet-tools hot-restart --force")
}

func newHotswapCmd() *cobra.Command {
	return withHelp(newCobraCmd("hotswap", "zero-downtime in-process binary reload", []string{"hot-swap"}, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdHotswap(rest, force, dryRun)
		})
	}), "Trigger an in-process zero-downtime HotSwap on a running provider without cycling the unit.", "  urnet-tools hotswap --unit urnetwork-native.service\n  urnet-tools hotswap --force")
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

func newSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "set",
		Short:              "runtime tuning override",
		Long:               "Read or write a runtime tuning override in the provider's state directory, applied live on the provider's next tick with no restart. Run with no key to list every active override, with just a key to show its current value, with a key and value to set it, or with a key and \"off\" to clear it back to the startup default. Run 'set help' to list the available keys (node-name, report-interval, proxy-url-max, proxy-url-refresh, cleanup-scope, cleanup-interval, fast-auth).",
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
	return &cobra.Command{
		Use:                "auth",
		Short:              "authenticate",
		Long:               "Authenticate the targeted provider against the URnetwork API by delegating to the provider binary's own auth subcommand. Pass an auth code, or omit it to use the provider's stored identity. Existing credentials are only overwritten with the provider's own -f flag, which passes through untouched.",
		Example:            "  urnet-tools auth\n  urnet-tools auth ABCD1234 --unit urnetwork-native.service",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdAuth(args)
		},
	}
}

func newChooseNetworkCmd() *cobra.Command {
	return &cobra.Command{
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
}

func newProxyCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "proxy",
		Short:              "Proxy Management",
		Long:               "Manage proxies for a provider: add from a file, paste from stdin/file/URL, clear, remove, refresh, and inspect health and traffic.",
		Example:            "  urnet-tools proxy add ~/proxies.txt           # Linux / macOS\n  urnet-tools proxy paste < proxies.txt         # Paste from stdin / pipe\n  urnet-tools proxy paste --file=~/proxies.txt  # Paste from file\n  urnet-tools proxy clear",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("proxy requires a subcommand: add <file> | paste | clear | remove | refresh | add-source <url> | remove-source <url> | health | traffic | ids | summary | remove-dead | trim <N>")
			}
			for _, a := range args {
				if a == "-h" || a == "--help" {
					return cmdProxy(args, false, false)
				}
			}
			return parseGlobal(args, func(force, dryRun bool, rest []string) error {
				return cmdProxy(rest, force, dryRun)
			})
		},
	}
}

func newReportCmd() *cobra.Command {
	return withHelp(newCobraCmd("report", "set hub report URL", nil, func(cmd *cobra.Command, args []string) error {
		rest, err := parseDelegationArgs(args)
		if err == errHelpShown {
			return nil
		}
		if err != nil {
			return err
		}
		return cmdReport(rest)
	}), "Set the hub report URL for one targeted provider at runtime, or pass \"off\" to disable reporting. This writes an override file the provider's bandwidth reporter re-reads on its next tick, so no restart is needed.", "  urnet-tools report http://192.0.2.10:8080 --unit urnetwork-native.service\n  urnet-tools report off --unit urnetwork-native.service")
}

func newHubCmd() *cobra.Command {
	return withHelp(newCobraCmd("hub", "Hub Management", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdHub(rest, force, dryRun)
		})
	}), "Manage the hub, which aggregates bandwidth reports from providers. set and off point a provider's reporting at a URL or remove it; install and init provision the hub service and its TLS certificate authority; link and unlink trust or untrust a hub from a provider; test verifies TLS connectivity; onboard-cmd mints a short-lived onboard token; show-password prints the CA password; update reinstalls the hub binary; open-port opens a firewall port for it. Most mutating actions ask for a typed \"yes\" unless you pass -f/--force.", "  urnet-tools hub set http://192.0.2.10:8080 --unit urnetwork-native.service\n  urnet-tools hub link https://hub.example.com:8443 --unit urnetwork-native.service\n  urnet-tools hub off --unit urnetwork-native.service")
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
		Use:   "show-ip",
		Short: "show the public IP on the dashboard label",
		Long: "Toggle or report whether the provider appends its public IP (fetched via ip.me) to the dashboard identity label set by `rename`. " +
			"Run with on, off, or status (the default with no argument). When off, the provider reports only the node name without an IP unless URNETWORK_PUBLIC_IP is set. " +
			"This controls what the dashboard displays, not which address the provider serves on; for that see `direct`.",
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
	return &cobra.Command{
		Use:                "history [limit]",
		Short:              "show command audit trail",
		Aliases:            []string{"audit"},
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			return cmdHistory(args)
		},
	}
}

func newMetricsCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "metrics on|off",
		Short:              "toggle Prometheus /metrics endpoint",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			return cmdMetrics(args, false)
		},
	}
}

func cmdHistory(args []string) error {
	limit := 50
	if len(args) > 0 {
		n, err := strconv.Atoi(args[0])
		if err != nil || n <= 0 {
			return fmt.Errorf("limit must be a positive integer (got %q)", args[0])
		}
		if n > 100 {
			n = 100
		}
		limit = n
	}

	t, _, err := parseTargetFlags(args)
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

	socketPath := filepath.Join(p.StateDir, "control.sock")
	resp, err := sendSocketRequest(socketPath, controlRequest{Cmd: "history", Limit: limit})
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
		ts := time.Unix(e.Timestamp, 0)
		if e.OK {
			fmt.Printf("[%s] OK  %s %s=%s\n", ts.Format("2006-01-02 15:04:05"), e.Cmd, e.Key, e.Value)
		} else {
			fmt.Printf("[%s] ERR %s %s=%s  error: %s\n", ts.Format("2006-01-02 15:04:05"), e.Cmd, e.Key, e.Value, e.Error)
		}
	}
	if resp.NextCursor != "" {
		fmt.Printf("\nMore entries available. Use 'urnet-tools history' with cursor %s for older entries.\n", resp.NextCursor)
	}
	return nil
}

func cmdMetrics(args []string, dryRun bool) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: urnet-tools metrics on|off")
	}
	val := strings.ToLower(args[0])
	switch val {
	case "on", "off":
	default:
		return fmt.Errorf("usage: urnet-tools metrics on|off (got %q)", args[0])
	}

	t, _, err := parseTargetFlags(args)
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

	socketPath := filepath.Join(p.StateDir, "control.sock")
	resp, err := sendSocketRequest(socketPath, controlRequest{Cmd: "set", Key: "metrics", Value: val})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("provider returned error: %s", resp.Error)
	}
	if resp.NeedsRestart {
		fmt.Printf("✓ Metrics %s (restart required for full effect)\n", val)
	} else {
		fmt.Printf("✓ Metrics %s\n", val)
	}
	return nil
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
		if err := queuePendingOverride(p.StateDir, "set", "profile", profile); err != nil {
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

	// Network identity
	fmt.Printf("  %sNetwork:%s %s (%s)\n", bold, reset, p.netLabel(), p.NetworkID[:8]+"...")

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

	// Pending restart indicators
	fmt.Println()
	restartKeys := []string{"profile", "ramlogs"}
	for _, key := range restartKeys {
		val, _, found, _ := queryControlOverride(p, key)
		if found && val != "" && val != "off" && val != "0" {
			fmt.Printf("  %s⚠ %s requires restart (%s)%s\n", yellow, key, val, reset)
		}
	}

	fmt.Println()
	fmt.Printf("  %s── Proxy Sources ──%s\n", dim, reset)
	fmt.Println()

	// Try to read proxy_url.json for source count
	proxyStatePath := ""
	if p.StateDir != "" {
		proxyStatePath = filepath.Join(p.StateDir, "proxy_url.json")
	}
	if proxyStatePath != "" {
		if data, err := os.ReadFile(proxyStatePath); err == nil {
			var sources []struct {
				Name   string `json:"name"`
				Source string `json:"source"`
			}
			if json.Unmarshal(data, &sources) == nil && len(sources) > 0 {
				for _, s := range sources {
					name := s.Name
					if name == "" {
						name = "(unnamed)"
					}
					fmt.Printf("  %s•%s %s %s(%s)%s\n", green, reset, name, dim, s.Source, reset)
				}
			} else {
				fmt.Printf("  %s(no proxy sources configured)%s\n", dim, reset)
			}
		}
	}

	fmt.Println()
	fmt.Printf("  %s── Quick Actions ──%s\n", dim, reset)
	fmt.Println()
	fmt.Printf("  urnet-tools set <key> <value>    Change a setting\n")
	fmt.Printf("  urnet-tools profile <name>       Switch tuning profile\n")
	fmt.Printf("  urnet-tools metrics on|off       Toggle Prometheus metrics\n")
	fmt.Printf("  urnet-tools history              View command audit trail\n")
	if p.Running && needsRestartNeeded(p) {
		fmt.Printf("\n  %s⚠ Restart pending: systemctl --user restart %s%s\n", yellow, p.Unit, reset)
	}
	fmt.Println()

	return nil
}

// needsRestartNeeded checks if any startup-only setting has been changed.
func needsRestartNeeded(p Provider) bool {
	restartKeys := []string{"profile", "ramlogs"}
	for _, key := range restartKeys {
		val, _, found, _ := queryControlOverride(p, key)
		if found && val != "" && val != "off" && val != "0" {
			return true
		}
	}
	return false
}
