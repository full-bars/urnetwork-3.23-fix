package urnettools

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// buildDockerRootCmd creates the root Cobra command for urnet-docker.

// withHelp sets a per-command Long description and usage Example so the
// command's own `-h` page is genuinely useful (Cobra benefit not delivered by
// the routing-only migration).
func withHelp(cmd *cobra.Command, long, example string) *cobra.Command {
	cmd.Long = long
	if example != "" {
		cmd.Example = example
	}
	return cmd
}

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
	return withHelp(newCobraCmd("providers", "list all provider containers", []string{"list", "ps"}, func(cmd *cobra.Command, args []string) error {
		return cmdDockerProviders(args)
	}), "List every provider container on the host, identified by its in-container JWT.", "  urnet-docker providers")
}

func newDockerStatusCmd() *cobra.Command {
	return withHelp(newCobraCmd("status [target]", "detailed status of one container", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerStatus(args)
	}), "Show detailed status of one provider container: running state, version, network, and resource usage.", "  urnet-docker status\n  urnet-docker status --network tacogonzalez3000")
}

func newDockerStartCmd() *cobra.Command {
	return withHelp(newCobraCmd("start [target]", "start container", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdDockerStart(rest, force, dryRun)
		})
	}), "Start a stopped provider container.", "  urnet-docker start --unit urnet-test")
}

func newDockerStopCmd() *cobra.Command {
	return withHelp(newCobraCmd("stop [target]", "stop container", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdDockerStop(rest, force, dryRun)
		})
	}), "Stop a running provider container.", "  urnet-docker stop --unit urnet-test")
}

func newDockerRestartCmd() *cobra.Command {
	return withHelp(newCobraCmd("restart [target]", "restart container", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdDockerRestart(rest, force, dryRun)
		})
	}), "Restart a running provider container.", "  urnet-docker restart --unit urnet-test")
}

func newDockerLogsCmd() *cobra.Command {
	return withHelp(newCobraCmd("logs [target] [N]", "follow container logs", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerLogs(args)
	}), "Follow a provider container's logs (RAMLOGS-aware /dev/shm fallback). Optionally limit with a line count.", "  urnet-docker logs --unit urnet-test\n  urnet-docker logs urnet-test 200")
}

func newDockerAuthCmd() *cobra.Command {
	return withHelp(newCobraCmd("auth [<code>] [target]", "authenticate provider inside container", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerAuth(args)
	}), "Authenticate the provider inside a container with an auth code.", "  urnet-docker auth <CODE> --unit urnet-test")
}

func newDockerChooseNetworkCmd() *cobra.Command {
	return withHelp(newCobraCmd("choose-network", "set API/connect endpoints inside container", []string{"choose_network"}, func(cmd *cobra.Command, args []string) error {
		return cmdDockerChooseNetwork(args)
	}), "Set the API and connect endpoints used by the provider inside the container.", "  urnet-docker choose-network <api> <connect>\n  urnet-docker choose-network --reset")
}

func newDockerSummaryCmd() *cobra.Command {
	return withHelp(newCobraCmd("summary [target]", "activity & performance summary", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerSummary(args)
	}), "Show an activity and performance summary for a provider container.", "  urnet-docker summary --unit urnet-test")
}

func newDockerReportCmd() *cobra.Command {
	return withHelp(newCobraCmd("report <url> [target]", "set hub report URL inside container", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerReport(args)
	}), "Set the hub report URL used by a provider container (no restart).", "  urnet-docker report https://hub.example.com")
}

func newDockerUpdateCmd() *cobra.Command {
	return withHelp(newCobraCmd("update [--unit <name>]", "update the host urnet-docker binary, or a container's provider in place (no recreate)", []string{"self-update", "selfupdate"}, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			// self-update/selfupdate are ALWAYS host-only: they must never be
			// routed at a target (Opus HIGH #453). Only the literal 'update'
			// command may target a container.
			if cmd.CalledAs() != "update" {
				return cmdSelfUpdate(rest, force, dryRun)
			}
			return cmdDockerUpdate(rest, force, dryRun)
		})
	}), "Update the host urnet-docker binary, or with a target flag update a provider container in place (no recreate).", "  urnet-docker update\n  urnet-docker update --unit urnet-test\n  urnet-docker update --unit=urnet-test")
}

func newDockerVersionCmd() *cobra.Command {
	return newCobraCmd("version", "print tool version", nil, func(cmd *cobra.Command, args []string) error {
		fmt.Println(ToolVersion)
		return nil
	})
}

// dockerProxySub builds one `proxy <sub>` cobra command. It forwards its
// own args plus the subcommand name to the shared proxy dispatcher
// (cmdDockerProxy), which owns target resolution + the in-container exec.
// DisableFlagParsing keeps intact the flags that belong to the container
// command; -h/--help inside the subcommand render that subcommand's help.
func dockerProxySub(sub, use, short, long, example string) *cobra.Command {
	return &cobra.Command{
		Use:                use,
		Short:              short,
		Long:               long,
		Example:            example,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			return cmdDockerProxy(append([]string{sub}, args...))
		},
	}
}

func newDockerProxyCmd() *cobra.Command {
	proxy := &cobra.Command{
		Use:                "proxy COMMAND [target]",
		Short:              "Proxy Management",
		Long:               "Manage proxies for a provider container: add from a host file or a URL source, prune, and inspect health and traffic.",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 || hasHelpFlag(args) || (len(args) == 1 && args[0] == "help") {
				return cmd.Help() // bare `proxy` / `proxy -h` / `proxy help`: show the proxy subcommand list
			}
			return cmdDockerProxy(args) // unknown subcommand -> dispatcher reports it
		},
	}
	proxy.AddCommand(
		dockerProxySub("add", "add <file> [target]", "copy a host proxy file and bulk-add to the container", "Copy a host proxy file (host:port[:user:pass] lines) into the container and bulk-add it.",
			"  urnet-docker proxy add ~/proxies.txt\n"+
				"  urnet-docker proxy add ~/proxies.txt --unit urnet-test"),
		dockerProxySub("clear", "clear [target] [--force]", "remove configured proxies", "Remove all configured proxies from the container.",
			"  urnet-docker proxy clear\n"+
				"  urnet-docker proxy clear --unit urnet-test --force"),
		dockerProxySub("remove", "remove [target] <proxy...> [--all]", "remove specific proxies", "Remove specific configured proxies (or all with --all).",
			"  urnet-docker proxy remove 1.2.3.4:5555\n"+
				"  urnet-docker proxy remove --all"),
		dockerProxySub("add-source", "add-source <url> [target]", "add a URL proxy source", "Add a remote URL that supplies proxy entries.", "  urnet-docker proxy add-source https://example.com/proxies.txt"),
		dockerProxySub("remove-source", "remove-source <url> [target]", "remove a URL proxy source", "Remove a URL proxy source.", "  urnet-docker proxy remove-source https://example.com/proxies.txt"),
		dockerProxySub("refresh", "refresh [target]", "hot-reload proxy sources", "Re-fetch URL sources and reload live proxies.",
			"  urnet-docker proxy refresh\n"+
				"  urnet-docker proxy refresh --unit urnet-test"),
		dockerProxySub("remove-dead", "remove-dead [target]", "prune dead/degraded proxies", "Remove dead and degraded proxies.", "  urnet-docker proxy remove-dead"),
		dockerProxySub("health", "health [target]", "show proxy health and live event log", "Show dead/degraded proxy health and the live health event log.", "  urnet-docker proxy health"),
		dockerProxySub("traffic", "traffic [target]", "real-time bandwidth and client load", "Show real-time bandwidth and client session load.", "  urnet-docker proxy traffic"),
		dockerProxySub("summary", "summary [target]", "proxy activity and performance summary", "Show a per-proxy activity and performance summary.", "  urnet-docker proxy summary"),
		dockerProxySub("trim", "trim <N|off> [target]", "hold running proxies at N, shed worst first", "Hard-cap running proxies at N, shedding the worst-graded (F -> A) first.",
			"  urnet-docker proxy trim 50\n"+
				"  urnet-docker proxy trim off"),
		dockerProxySub("exclude", "exclude [<pattern>] [target]", "exclude proxies matching a pattern", "Exclude proxies matching a pattern (show current exclusions with no argument).", "  urnet-docker proxy exclude 1.2.3.4"),
	)
	return proxy
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
		Use:                "exec [target] [--] <cmd...>",
		Short:              "run arbitrary command inside container",
		Long:               "Run an arbitrary command inside a provider container. Target flags (--unit/--network/etc) must precede the command; use \" -- \" to forward inner flags verbatim to the container command so they are never mistaken for urnet-docker flags.",
		Example:            "  urnet-docker exec --unit urnet-test -- urnet-tools proxy add --proxy_file=/tmp/p.txt",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Pre-separator help is urnet-docker's own; render exec's help.
			if _, _, err := splitExecArgs(args); err == errHelpShown {
				return cmd.Help()
			}
			return cmdDockerExec(args)
		},
	}
}
