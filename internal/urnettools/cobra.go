package urnettools

import (
	"fmt"
	"os"

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

Session & Identity:
  session save <file>     Export identity + proxy state (encrypted)
  session load <file>     Import identity + proxy state, then restart
  auth [<code>]           Authenticate (omit for interactive paste)
  choose-network          Set API/connect endpoints
  default                 Persist a default provider target for this box

Proxy Management:
  proxy add <file>        Bulk add proxies from a text file
  proxy clear             Remove all configured proxies
  proxy remove            Remove proxies (by addr/match, or all)
  proxy refresh [--force] Re-read configs and hot-reload proxies
  proxy trim <N>          Hold running proxies at N, shed worst first
  proxy health            Show dead/degraded proxies + live event log
  proxy traffic           Real-time bandwidth & client session load
  proxy summary           Fleet-style summary (sources, health, counts)
  proxy remove-dead       Prune dead/degraded/failing proxies interactively
  report [<url>|off]      Set hub report URL
  self-heal [on|off]      Auto-regulate proxies (load gate + cleanup)
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
		newStartCmd(),
		newStopCmd(),
		newRestartCmd(),
		newUpdateCmd(),
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
	return withHelp(newCobraCmd("providers", "list all providers on this box", []string{"list", "ps"}, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdProviders(rest)
		})
	}), "List every provider found on this box: systemd units and bare processes, across all OS users, identified by their JWT network identity. If no systemd providers exist but provider containers do, it says so and points you at urnet-docker.", "  urnet-tools providers")
}

func newStatusCmd() *cobra.Command {
	return withHelp(newCobraCmd("status [target]", "detailed status of one provider", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdStatus(rest)
		})
	}), "Show detailed status for one provider: user, unit, binary, version, state dir, PID, running state, network identity, and JWT expiry. On Linux this reproduces `systemctl status <unit>`; on Windows and macOS it renders a status panel with a proxy summary. Target a specific provider with --unit, --user, --network, --network-id, or --state-dir.", "  urnet-tools status\n  urnet-tools status --network tacogonzalez3000\n  urnet-tools status --unit urnetwork-native.service")
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
	}), "Restart the provider's systemd unit. This is a production action, so it asks for a typed \"yes\" unless you pass -f/--force. Use -n/--dry-run to print the plan without acting.", "  urnet-tools restart --unit urnetwork-native.service\n  urnet-tools restart --network tacogonzalez3000 --force")
}

func newUpdateCmd() *cobra.Command {
	return withHelp(newCobraCmd("update [target]", "update provider(s) to latest", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdUpdate(rest, force, dryRun)
		})
	}), "Download and install the latest provider release, verify its sha256 digest, swap the binary, and restart the owning unit. With no target and multiple providers it prompts interactively; use --all to update every provider, or --include/--exclude to pick a subset. Pin a release with --tag, or an exact asset with --digest and --url. This also refreshes the urnet-tools binary itself from the same release.", "  urnet-tools update\n  urnet-tools update --unit urnetwork-native.service\n  urnet-tools update --all --force\n  urnet-tools update --tag v3.23.0-fix.30.5")
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
	}), "Set the throughput profile to v4 or v8 to raise limits on a RAM-rich box, or turn it off to clear the override. This writes a systemd drop-in and restarts the provider unit, so it asks for a typed \"yes\" unless you pass -f/--force. Target a specific provider with --unit, --user, --network, or --network-id.", "  urnet-tools turbo v8\n  urnet-tools turbo off --unit urnetwork-native.service")
}

func newAutoCmd() *cobra.Command {
	return withHelp(newCobraCmd("auto", "AUTO-TUNE detect hardware and pick best profile", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdTune("auto", rest, force, dryRun)
		})
	}), "Turn on or off the auto-tuning profile, which lets the provider detect the box's hardware and pick the best-fit performance profile. This writes a systemd drop-in and restarts the provider unit, so it asks for a typed \"yes\" unless you pass -f/--force.", "  urnet-tools auto on\n  urnet-tools auto off --unit urnetwork-native.service")
}

func newEcoCmd() *cobra.Command {
	return withHelp(newCobraCmd("eco", "ECO MODE GC-tuned for low-RAM systems", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdTune("eco", rest, force, dryRun)
		})
	}), "Turn on or off eco mode, a garbage-collection-tuned profile for low-RAM systems. This writes a systemd drop-in and restarts the provider unit, so it asks for a typed \"yes\" unless you pass -f/--force.", "  urnet-tools eco on\n  urnet-tools eco off --user urnet")
}

func newLowmodeCmd() *cobra.Command {
	return withHelp(newCobraCmd("lowmode", "LOW-MEMORY reduced buffers for max RAM savings", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdTune("lowmode", rest, force, dryRun)
		})
	}), "Turn on or off low-memory mode, which reduces buffers to save RAM at the cost of throughput. This writes a systemd drop-in and restarts the provider unit, so it asks for a typed \"yes\" unless you pass -f/--force.", "  urnet-tools lowmode on\n  urnet-tools lowmode off --unit urnetwork-native.service")
}

func newRamlogsCmd() *cobra.Command {
	return withHelp(newCobraCmd("ramlogs", "RAM LOGS zero disk I/O logging", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdTune("ramlogs", rest, force, dryRun)
		})
	}), "Turn on or off RAM logging, which writes provider logs to a RAM buffer instead of disk. This writes a systemd drop-in and restarts the provider unit, so it asks for a typed \"yes\" unless you pass -f/--force.", "  urnet-tools ramlogs on\n  urnet-tools ramlogs off --network tacogonzalez3000")
}

func newOptimizeCmd() *cobra.Command {
	return withHelp(newCobraCmd("optimize", "apply golden-fleet OS/kernel limits", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdOptimize(rest, force, dryRun)
		})
	}), "Apply golden-fleet OS and kernel network limits to this host: socket buffers, file descriptor limit, ephemeral port range, and TIME_WAIT timeout on Linux, or the netsh and registry equivalents on Windows. This is host-wide, not per provider, so no target flag applies. It asks for a typed \"yes\" unless you pass -f/--force, and needs root (or sudo) on Linux.", "  urnet-tools optimize\n  sudo urnet-tools optimize --force")
}

func newHotRestartCmd() *cobra.Command {
	return withHelp(newCobraCmd("hot-restart", "restart provider (hot-restart is a config toggle)", []string{"hotrestart"}, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdHotRestart(rest, force, dryRun)
		})
	}), "Restart the provider's unit in a way that lets it reuse client IDs across the restart. It takes no extra arguments beyond a target, and asks for a typed \"yes\" unless you pass -f/--force.", "  urnet-tools hot-restart --unit urnetwork-native.service\n  urnet-tools hot-restart --force")
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
		Long:               "Manage proxies for a provider: add from a file, clear, remove, refresh, and inspect health and traffic.",
		Example:            "  urnet-tools proxy add ~/proxies.txt           # Linux / macOS\n  urnet-tools proxy add C:\\Users\\<you>\\proxies.txt    # Windows (\\ or / separators)\n  urnet-tools proxy clear",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("proxy requires a subcommand: add <file> | clear | remove | refresh | add-source <url> | remove-source <url> | health | traffic | summary | remove-dead | trim <N> | exclude")
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
