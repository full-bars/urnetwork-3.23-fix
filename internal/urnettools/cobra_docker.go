package urnettools

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// buildDockerRootCmd creates the root Cobra command for urnet-docker.
func buildDockerRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "urnet-docker",
		Short: "docker-container URnetwork manager",
		Long:  "urnet-docker — docker-container URnetwork manager",
		SilenceUsage: true,
		SilenceErrors: true,
	}
	rootCmd.SetOut(os.Stderr)
	rootCmd.SetErr(os.Stderr)

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
	return newCobraCmd("version", "print tool version", []string{"-v", "--version"}, func(cmd *cobra.Command, args []string) error {
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
				return fmt.Errorf("proxy requires a subcommand: add <file> | clear | remove | add-source <url> | remove-source <url> | refresh | remove-dead | health | traffic | summary | trim <N> | exclude")
			}
			for _, a := range args {
				if a == "-h" || a == "--help" {
					// In docker, help flag triggers usageDocker() in the original CLI.
					// But wait, the original `cmdDockerProxy` didn't print help, it was printed by `usageDocker()`.
					// We can just let Cobra print the help. 
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
	return newCobraCmd("exec", "run arbitrary command inside container", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerExec(args)
	})
}
