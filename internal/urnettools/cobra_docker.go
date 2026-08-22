package urnettools

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// buildDockerRootCmd creates the root Cobra command for urnet-docker.
func buildDockerRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:           "urnet-docker",
		Short:         "docker-container URnetwork manager",
		Long:          "urnet-docker — docker-container URnetwork manager",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	rootCmd.SetOut(os.Stderr)
	rootCmd.SetErr(os.Stderr)
	rootCmd.CompletionOptions.DisableDefaultCmd = true

	rootCmd.PersistentFlags().String("unit", "", "container name (mapped to Unit)")
	rootCmd.PersistentFlags().String("network", "", "JWT network name, e.g. tacogonzalez3000")
	rootCmd.PersistentFlags().String("network-id", "", "JWT network id")
	rootCmd.PersistentFlags().String("state-dir", "", "state dir INSIDE the container")
	rootCmd.PersistentFlags().BoolP("force", "f", false, "bypass the confirm gate")
	rootCmd.PersistentFlags().BoolP("dry-run", "n", false, "show what would happen without doing it")

	rootCmd.AddCommand(
		newDockerProvidersCmd(),
		newDockerStatusCmd(),
		newDockerStartCmd(),
		newDockerStopCmd(),
		newDockerRestartCmd(),
		newDockerLogsCmd(),
		newDockerAuthCmd(),
		newDockerChooseNetworkCmd(),
		newDockerSummaryCmd(),
		newDockerReportCmd(),
		newDockerUpdateCmd(),
		newDockerVersionCmd(),
		newDockerProxyCmd(),
		newDockerSelfHealCmd(),
		newDockerSetCmd(),
		newDockerFastAuthCmd(),
		newDockerHubCmd(),
		newDockerSessionCmd(),
		newDockerExecCmd(),
	)

	return rootCmd
}

func newDockerProvidersCmd() *cobra.Command {
	return newCobraCmd("providers", "list all provider containers", []string{"list", "ps"}, func(cmd *cobra.Command, args []string) error {
		return cmdDockerProviders(args)
	})
}

func newDockerStatusCmd() *cobra.Command {
	return newCobraCmd("status [target]", "detailed status of one container", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerStatus(args)
	})
}

func newDockerStartCmd() *cobra.Command {
	return newCobraCmd("start [target]", "start container", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdDockerStart(rest, force, dryRun)
		})
	})
}

func newDockerStopCmd() *cobra.Command {
	return newCobraCmd("stop [target]", "stop container", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdDockerStop(rest, force, dryRun)
		})
	})
}

func newDockerRestartCmd() *cobra.Command {
	return newCobraCmd("restart [target]", "restart container", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdDockerRestart(rest, force, dryRun)
		})
	})
}

func newDockerLogsCmd() *cobra.Command {
	return newCobraCmd("logs [target] [N]", "follow container logs", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerLogs(args)
	})
}

func newDockerAuthCmd() *cobra.Command {
	return newCobraCmd("auth [<code>] [target]", "authenticate provider inside container", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerAuth(args)
	})
}

func newDockerChooseNetworkCmd() *cobra.Command {
	return newCobraCmd("choose-network", "set API/connect endpoints inside container", []string{"choose_network"}, func(cmd *cobra.Command, args []string) error {
		return cmdDockerChooseNetwork(args)
	})
}

func newDockerSummaryCmd() *cobra.Command {
	return newCobraCmd("summary [target]", "activity & performance summary", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerSummary(args)
	})
}

func newDockerReportCmd() *cobra.Command {
	return newCobraCmd("report <url> [target]", "set hub report URL inside container", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerReport(args)
	})
}

func newDockerUpdateCmd() *cobra.Command {
	return newCobraCmd("update", "update urnet-docker binary on host", []string{"self-update", "selfupdate"}, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdSelfUpdate(rest, force, dryRun)
		})
	})
}

func newDockerVersionCmd() *cobra.Command {
	return newCobraCmd("version", "print tool version", nil, func(cmd *cobra.Command, args []string) error {
		fmt.Println(ToolVersion)
		return nil
	})
}

func newDockerProxyCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "proxy",
		Short:              "Proxy Management",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				// Bare `proxy` prints the subcommand list and exits 0, matching the
				// pre-cobra dispatcher (exit 1 broke scripts that call it to show
				// the menu, review MEDIUM).
				fmt.Fprintln(os.Stderr, "proxy requires a subcommand: add <file> | clear | remove | add-source <url> | remove-source <url> | refresh | remove-dead | health | traffic | summary | trim <N> | exclude")
				return nil
			}
			for _, a := range args {
				if a == "-h" || a == "--help" {
					// Let Cobra print the proxy help on an explicit -h/--help.
					return cmd.Help()
				}
			}
			return cmdDockerProxy(args)
		},
	}
}

func newDockerSelfHealCmd() *cobra.Command {
	return newCobraCmd("self-heal", "manage automatic proxy self-healing", []string{"selfheal"}, func(cmd *cobra.Command, args []string) error {
		return cmdDockerSelfHeal(args)
	})
}

func newDockerSetCmd() *cobra.Command {
	return newCobraCmd("set", "runtime tuning override in container state", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerSet(args)
	})
}

func newDockerFastAuthCmd() *cobra.Command {
	return newCobraCmd("fast-auth", "manage auth rate limiter bypass marker", []string{"fastauth"}, func(cmd *cobra.Command, args []string) error {
		return cmdDockerFastAuth(args)
	})
}

func newDockerHubCmd() *cobra.Command {
	return newCobraCmd("hub", "delegate hub management commands", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerHub(args)
	})
}

func newDockerSessionCmd() *cobra.Command {
	return newCobraCmd("session", "export/import encrypted identity+proxy bundle", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerSession(args)
	})
}

func newDockerExecCmd() *cobra.Command {
	// MUST NOT use newCobraCmd: its broad hasHelpFlag intercepts '--help' AFTER
	// the '--' separator, which belongs to the container command being run
	// (review CRITICAL - help-after-sep must be forwarded). splitExecArgs decides
	// what is help; delegate straight through.
	return &cobra.Command{
		Use:                "exec",
		Short:              "run arbitrary command inside container",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdDockerExec(args)
		},
	}
}
