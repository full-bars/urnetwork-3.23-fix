package urnettools

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

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
		newReportCmd(),
		newHubCmd(),
		newReinstallCmd(),
		newUninstallCmd(),
		newAutoUpdateCmd(),
		newAutoStartCmd(),
		newSelfHealCmd(),
	)

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
	}), "List every provider on this box with its unit, network, state dir and version.", "  urnet-tools providers")
}

func newStatusCmd() *cobra.Command {
	return withHelp(newCobraCmd("status [target]", "detailed status of one provider", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdStatus(rest)
		})
	}), "Show detailed status of one provider: running state, version, connectivity, and resource usage.", "  urnet-tools status\n  urnet-tools status --network tacogonzalez3000")
}

func newStartCmd() *cobra.Command {
	return withHelp(newCobraCmd("start [target]", "start the provider's systemd unit", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdStart(rest, force, dryRun)
		})
	}), "Start the provider's systemd unit.", "  urnet-tools start --unit urnetwork-native.service")
}

func newStopCmd() *cobra.Command {
	return withHelp(newCobraCmd("stop [target]", "stop the provider's systemd unit", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdStop(rest, force, dryRun)
		})
	}), "Stop the provider's systemd unit.", "  urnet-tools stop --unit urnetwork-native.service")
}

func newRestartCmd() *cobra.Command {
	return withHelp(newCobraCmd("restart [target]", "restart the provider's systemd unit", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdRestart(rest, force, dryRun)
		})
	}), "Restart the provider's systemd unit.", "  urnet-tools restart --unit urnetwork-native.service")
}

func newUpdateCmd() *cobra.Command {
	return withHelp(newCobraCmd("update [target]", "update provider(s) to latest", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdUpdate(rest, force, dryRun)
		})
	}), "Update the selected provider(s) to the latest version.", "  urnet-tools update\n  urnet-tools update --unit urnetwork-native.service")
}

func newSelfUpdateCmd() *cobra.Command {
	return withHelp(newCobraCmd("self-update", "update this tool binary itself", []string{"selfupdate"}, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdSelfUpdate(rest, force, dryRun)
		})
	}), "update this tool binary itself to the latest version", "  urnet-tools self-update")
}

func newLogsCmd() *cobra.Command {
	return withHelp(newCobraCmd("logs [target] [N]", "show recent provider logs", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdLogs(rest)
		})
	}), "Show recent provider logs (optionally a line count).", "  urnet-tools logs\n  urnet-tools logs --unit urnetwork-native.service 200")
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
	}), "Show a fleet-style summary for one provider.", "  urnet-tools summary\n  urnet-tools summary --unit urnetwork-native.service")
}

func newVersionCmd() *cobra.Command {
	// '-v'/'--version' were dead aliases: Cobra strips '-' tokens before alias
	// matching, so they never resolve (handled at top level). Keep plain 'version'.
	return withHelp(newCobraCmd("version", "print this tool's version", nil, func(cmd *cobra.Command, args []string) error {
		fmt.Println(ToolVersion)
		return nil
	}), "Print this tool's version.", "  urnet-tools version")
}

func newDefaultCmd() *cobra.Command {
	return withHelp(newCobraCmd("default", "persist a default provider target for this box", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDefault(args)
	}), "Persist, show, or clear the default provider target for this box.", "  urnet-tools default set --unit urnetwork-native.service\n  urnet-tools default show\n  urnet-tools default clear")
}

func newSessionCmd() *cobra.Command {
	// cmdSession owns its rich help (save|load <file>, --allow-different-account);
	// building raw here lets that help fire instead of Cobra's stub (review MEDIUM).
	return &cobra.Command{
		Use:                "session",
		Short:              "export/import identity + proxy state",
		Long:               "Export or import an encrypted identity and proxy bundle.",
		Example:            "  urnet-tools session save bundle.bin\n  urnet-tools session load bundle.bin",
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
	}), "Raise throughput limits for RAM-rich boxes.", "  urnet-tools turbo v8\n  urnet-tools turbo off --unit urnetwork-native.service")
}

func newAutoCmd() *cobra.Command {
	return withHelp(newCobraCmd("auto", "AUTO-TUNE detect hardware and pick best profile", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdTune("auto", rest, force, dryRun)
		})
	}), "Auto-detect hardware and pick the best performance profile.", "  urnet-tools auto on\n  urnet-tools auto off")
}

func newEcoCmd() *cobra.Command {
	return withHelp(newCobraCmd("eco", "ECO MODE GC-tuned for low-RAM systems", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdTune("eco", rest, force, dryRun)
		})
	}), "Enable eco mode with GC tuning for low-RAM systems.", "  urnet-tools eco on\n  urnet-tools eco off")
}

func newLowmodeCmd() *cobra.Command {
	return withHelp(newCobraCmd("lowmode", "LOW-MEMORY reduced buffers for max RAM savings", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdTune("lowmode", rest, force, dryRun)
		})
	}), "Enable low-memory mode with reduced buffers for maximum RAM savings.", "  urnet-tools lowmode on\n  urnet-tools lowmode off")
}

func newRamlogsCmd() *cobra.Command {
	return withHelp(newCobraCmd("ramlogs", "RAM LOGS zero disk I/O logging", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdTune("ramlogs", rest, force, dryRun)
		})
	}), "Enable zero disk I/O logging backed by RAM.", "  urnet-tools ramlogs on\n  urnet-tools ramlogs off")
}

func newOptimizeCmd() *cobra.Command {
	return withHelp(newCobraCmd("optimize", "apply golden-fleet OS/kernel limits", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdOptimize(rest, force, dryRun)
		})
	}), "Apply golden-fleet OS and kernel limits.", "  urnet-tools optimize\n  urnet-tools optimize --unit urnetwork-native.service")
}

func newHotRestartCmd() *cobra.Command {
	return withHelp(newCobraCmd("hot-restart", "reuse client_ids across restarts", []string{"hotrestart"}, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdHotRestart(rest, force, dryRun)
		})
	}), "Reuse client IDs across provider restarts.", "  urnet-tools hot-restart --unit urnetwork-native.service")
}

func newFastAuthCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "fast-auth",
		Short:              "manage the auth rate limiter",
		Long:               "Manage the auth rate limiter bypass marker.",
		Example:            "  urnet-tools fast-auth on\n  urnet-tools fast-auth status",
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
		Long:               "Runtime tuning override in the provider state, read live without restart.",
		Example:            "  urnet-tools set <key> [<value>|off]",
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
		Long:               "Authenticate the provider with an auth code.",
		Example:            "  urnet-tools auth <CODE>\n  urnet-tools auth <CODE> --unit urnetwork-native.service",
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
		Long:               "Set the API and connect endpoints used by the provider.",
		Example:            "  urnet-tools choose-network <api> <connect>\n  urnet-tools choose-network --reset",
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
		Example:            "  urnet-tools proxy add ~/proxies.txt\n  urnet-tools proxy clear",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("proxy requires a subcommand: add <file> | clear | remove | refresh")
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
	}), "Set the hub report URL used by a provider (no restart).", "  urnet-tools report https://hub.example.com\n  urnet-tools report off")
}

func newHubCmd() *cobra.Command {
	return withHelp(newCobraCmd("hub", "Hub Management", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdHub(rest, force, dryRun)
		})
	}), "Manage the hub: install, init, link, unlink, test, update, and control reporting.", "  urnet-tools hub set host:port\n  urnet-tools hub off")
}

func newReinstallCmd() *cobra.Command {
	return withHelp(newCobraCmd("reinstall", "reinstall provider", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdReinstall(rest, force, dryRun)
		})
	}), "Reinstall the provider.", "  urnet-tools reinstall")
}

func newUninstallCmd() *cobra.Command {
	return withHelp(newCobraCmd("uninstall", "uninstall provider", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdUninstall(rest, force, dryRun)
		})
	}), "Uninstall the provider.", "  urnet-tools uninstall")
}

func newAutoUpdateCmd() *cobra.Command {
	return withHelp(newCobraCmd("auto-update", "manage auto-update schedule", []string{"autoupdate"}, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdAutoUpdate(rest, force, dryRun)
		})
	}), "Manage the auto-update schedule.", "  urnet-tools auto-update on\n  urnet-tools auto-update off")
}

func newAutoStartCmd() *cobra.Command {
	return withHelp(newCobraCmd("auto-start", "toggle auto-start on login", []string{"autostart"}, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdAutoStart(rest, force, dryRun)
		})
	}), "Toggle provider auto-start on login.", "  urnet-tools auto-start on\n  urnet-tools auto-start off")
}

func newSelfHealCmd() *cobra.Command {
	// cmdSelfHeal has its own -h handling; building raw preserves it (review MEDIUM).
	return &cobra.Command{
		Use:                "self-heal",
		Short:              "self heal",
		Long:               "Manage automatic proxy self-healing.",
		Example:            "  urnet-tools self-heal on\n  urnet-tools self-heal off",
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
