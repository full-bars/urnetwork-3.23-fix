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

// dockerRootHelpMenu is the curated root menu for urnet-docker, matching the
// tool's sectioned/emoji style. Replaces Cobra's flat list; per-command help
// pages are untouched (subcommands reset to the default template below).
const dockerRootHelpMenu = `urnet-docker — docker-container URnetwork manager

Usage:
  urnet-docker [command]

Core Commands:
  start                     Start container
  stop                      Stop container
  restart                   Restart container
  update                    Update host binary or provider in place (no recreate)
  status                    Detailed status of one container
  logs                      Follow container logs
  exec <cmd>                Run arbitrary command inside the container
  providers                 List all provider containers
  version                   Print this tool's version

Proxy Management (inside the container):
  proxy add <file>                Bulk add proxies
  proxy clear                     Remove all configured proxies
  proxy remove                    Remove proxies (by addr/match, or all)
  proxy refresh [--force]         Re-read configs and hot-reload
  proxy trim <N>                  Hold running proxies at N, shed worst first
  proxy add-source <url>          Add a URL proxy source
  proxy remove-source <url>       Remove a URL proxy source
  proxy health                    Show dead/degraded proxies
  proxy traffic                   Real-time bandwidth & client session load
  proxy summary                   Fleet-style summary (sources, health, counts)
  proxy remove-dead               Prune dead/degraded/failing proxies

Hub Management:
  hub ...                         hub CA, trust, report set/off, onboard, install, update

Config & Automation:
  auth [<code>]             Authenticate (interactive paste)
  choose-network            Set API/connect endpoints
  fast-auth [on|off]        Bypass auth rate limiter without restart
  set [<k> [<v>|off]]       Show or change runtime tuning overrides
  self-heal [on|off]        Auto-regulate proxies (load gate + cleanup)
  session save|load <file>  Export/import identity + proxy state (encrypted)
  report <url>|off          Set hub report URL
  help <command>            Show help for a command

Targeting (used when more than one container exists):
  --unit <container>        container name (mapped to Unit)
  --network <name>          JWT network name
  --network-id <id>         JWT network id (true unique identity)
  --state-dir <dir>         state dir INSIDE the container

Batch / safety flags:
  -f, --force                 bypass the confirm gate
  -n, --dry-run               show what would happen without doing it
  -h, --help                  show help (never executes)

Need help? Email support@fullbars.xyz or visit https://github.com/full-bars/urnetwork-3.23-fix
`

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
	// Curated root menu for bare urnet-docker / root -h; subcommands keep
	// their own per-command Cobra help page (reset below).
	rootCmd.Run = func(cmd *cobra.Command, args []string) {
		fmt.Fprint(cmd.OutOrStderr(), dockerRootHelpMenu)
	}
	rootCmd.SetHelpTemplate(dockerRootHelpMenu)

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
	for _, sub := range rootCmd.Commands() {
		sub.SetHelpTemplate(`{{with (or .Long .Short)}}{{. | trimTrailingWhitespaces}}
{{end}}{{if or .Runnable .HasSubCommands}}{{.UsageString}}{{end}}`)
	}

	return rootCmd
}

func newDockerProvidersCmd() *cobra.Command {
	return withHelp(newCobraCmd("providers", "list all provider containers", []string{"list", "ps"}, func(cmd *cobra.Command, args []string) error {
		return cmdDockerProviders(args)
	}), "List every provider container on this host, identified by its in-container JWT identity.", "  urnet-docker providers")
}

func newDockerStatusCmd() *cobra.Command {
	return withHelp(newCobraCmd("status [target]", "detailed status of one container", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerStatus(args)
	}), "Show detailed status for one provider container: image, running state, in-container state dir, network identity, and JWT expiry. Target it with --unit (the container name), --network, --network-id, --state-dir, or a bare container name.", "  urnet-docker status\n  urnet-docker status mynetwork-provider\n  urnet-docker status --network tacogonzalez3000")
}

func newDockerStartCmd() *cobra.Command {
	return withHelp(newCobraCmd("start [target]", "start container", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdDockerStart(rest, force, dryRun)
		})
	}), "Start a stopped provider container. Asks for a typed \"yes\" unless you pass -f/--force; -n/--dry-run prints the plan without acting.", "  urnet-docker start --unit mynetwork-provider\n  urnet-docker start mynetwork-provider")
}

func newDockerStopCmd() *cobra.Command {
	return withHelp(newCobraCmd("stop [target]", "stop container", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdDockerStop(rest, force, dryRun)
		})
	}), "Stop a running provider container. Asks for a typed \"yes\" unless you pass -f/--force.", "  urnet-docker stop --unit mynetwork-provider")
}

func newDockerRestartCmd() *cobra.Command {
	return withHelp(newCobraCmd("restart [target]", "restart container", nil, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			return cmdDockerRestart(rest, force, dryRun)
		})
	}), "Restart a provider container. Asks for a typed \"yes\" unless you pass -f/--force.", "  urnet-docker restart --unit mynetwork-provider")
}

func newDockerLogsCmd() *cobra.Command {
	return withHelp(newCobraCmd("logs [target] [N]", "follow container logs", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerLogs(args)
	}), "Follow a provider container's logs. The default is the last 250 lines; pass a number to change it. When the container writes RAMLOGS to /dev/shm, this streams that file instead of docker logs.", "  urnet-docker logs --unit mynetwork-provider\n  urnet-docker logs mynetwork-provider 200")
}

func newDockerAuthCmd() *cobra.Command {
	return withHelp(newCobraCmd("auth [<code>] [target]", "authenticate provider inside container", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerAuth(args)
	}), "Authenticate the provider running inside the targeted container by delegating to its own urnet-tools auth. Pass an auth code, or omit it to use the container's stored identity.", "  urnet-docker auth --unit mynetwork-provider\n  urnet-docker auth ABCD1234 mynetwork-provider")
}

func newDockerChooseNetworkCmd() *cobra.Command {
	return withHelp(newCobraCmd("choose-network", "set API/connect endpoints inside container", []string{"choose_network"}, func(cmd *cobra.Command, args []string) error {
		return cmdDockerChooseNetwork(args)
	}), "Set the API and connect endpoints the provider inside the targeted container uses, or clear the override with --reset. Delegates to the container's own urnet-tools choose-network.", "  urnet-docker choose-network https://api.example.com wss://connect.example.com --unit mynetwork-provider\n  urnet-docker choose-network --reset --unit mynetwork-provider")
}

func newDockerSummaryCmd() *cobra.Command {
	return withHelp(newCobraCmd("summary [target]", "activity & performance summary", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerSummary(args)
	}), "Show an activity and performance summary for the provider inside the targeted container.", "  urnet-docker summary --unit mynetwork-provider")
}

func newDockerReportCmd() *cobra.Command {
	return withHelp(newCobraCmd("report <url> [target]", "set hub report URL inside container", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerReport(args)
	}), "Set the hub report URL used by the provider inside the targeted container, applied without a restart.", "  urnet-docker report https://hub.example.com --unit mynetwork-provider")
}

func newDockerUpdateCmd() *cobra.Command {
	return withHelp(newCobraCmd("update [<container> | --unit <name>]", "update the host urnet-docker binary, or a container's provider in place (no recreate)", []string{"self-update", "selfupdate"}, func(cmd *cobra.Command, args []string) error {
		return parseGlobal(args, func(force, dryRun bool, rest []string) error {
			// self-update/selfupdate are ALWAYS host-only: they must never be
			// routed at a target. Only the literal 'update' command targets a container.
			if cmd.CalledAs() != "update" {
				return cmdSelfUpdate(rest, force, dryRun)
			}
			return cmdDockerUpdate(rest, force, dryRun)
		})
	}), "With a target (a container name given directly, or --unit/--network/--network-id/--state-dir), update that container's provider binary in place with no recreate, by running its own urnet-tools update inside the container; the container must already be running. With no target, this instead self-updates the host urnet-docker binary, so --tag/--digest/--url then apply to the host tool, not any container.", "  urnet-docker update\n  urnet-docker update mynetwork-provider\n  urnet-docker update --unit mynetwork-provider\n  urnet-docker update --unit=mynetwork-provider")
}

func newDockerVersionCmd() *cobra.Command {
	return withHelp(newCobraCmd("version", "print tool version", nil, func(cmd *cobra.Command, args []string) error {
		fmt.Println(ToolVersion)
		return nil
	}), "Print the urnet-docker build version and exit.", "  urnet-docker version")
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
		dockerProxySub("add", "add <file> [target]", "copy a host proxy file and bulk-add to the container", "Copy a host proxy file (host:port, or host:port:user:pass per line) into the targeted container and bulk-add it to the provider running there.",
			"  urnet-docker proxy add ~/proxies.txt              # Linux / macOS\n"+
				"  urnet-docker proxy add C:\\Users\\<you>\\proxies.txt   # Windows (\\ or / separators)\n"+
				"  urnet-docker proxy add ~/proxies.txt --unit mynetwork-provider"),
		dockerProxySub("clear", "clear [target] [--force]", "remove configured proxies", "Remove all configured proxies from the provider inside the targeted container. Any extra flags, such as --force, are forwarded so this is scriptable from a non-interactive job.",
			"  urnet-docker proxy clear --unit mynetwork-provider\n"+
				"  urnet-docker proxy clear --unit mynetwork-provider --force"),
		dockerProxySub("remove", "remove [target] <proxy...> [--all]", "remove specific proxies", "Remove specific proxies from the targeted container's provider by address, or every proxy with --all.",
			"  urnet-docker proxy remove 1.2.3.4:5555 --unit mynetwork-provider\n"+
				"  urnet-docker proxy remove --all --unit mynetwork-provider"),
		dockerProxySub("add-source", "add-source <url> [target]", "add a URL proxy source", "Add a URL proxy source to the provider inside the targeted container.", "  urnet-docker proxy add-source https://example.com/proxies.txt --unit mynetwork-provider"),
		dockerProxySub("remove-source", "remove-source <url> [target]", "remove a URL proxy source", "Remove a URL proxy source from the provider inside the targeted container.", "  urnet-docker proxy remove-source https://example.com/proxies.txt --unit mynetwork-provider"),
		dockerProxySub("refresh", "refresh [target]", "hot-reload proxy sources", "Re-fetch URL proxy sources and hot-reload proxies for the provider inside the targeted container.",
			"  urnet-docker proxy refresh --unit mynetwork-provider"),
		dockerProxySub("remove-dead", "remove-dead [target]", "prune dead/degraded proxies", "Remove dead and degraded proxies from the provider inside the targeted container.", "  urnet-docker proxy remove-dead --unit mynetwork-provider"),
		dockerProxySub("health", "health [target]", "show proxy health and live event log", "Show proxy health and stream the live health event log for the provider inside the targeted container.", "  urnet-docker proxy health --unit mynetwork-provider"),
		dockerProxySub("traffic", "traffic [target]", "real-time bandwidth and client load", "Show real-time bandwidth and client session load for the provider inside the targeted container.", "  urnet-docker proxy traffic --unit mynetwork-provider"),
		dockerProxySub("summary", "summary [target]", "proxy activity and performance summary", "Show a per-proxy activity and performance summary for the provider inside the targeted container.", "  urnet-docker proxy summary --unit mynetwork-provider"),
		dockerProxySub("trim", "trim <N|off> [target]", "hold running proxies at N, shed worst first", "Hold running proxies at N for the provider inside the targeted container, shedding the worst-graded (F to A) first, or pass \"off\" to clear the cap.",
			"  urnet-docker proxy trim 50 --unit mynetwork-provider\n"+
				"  urnet-docker proxy trim off --unit mynetwork-provider"),
		dockerProxySub("exclude", "exclude [<pattern>] [target]", "exclude proxies matching a pattern", "Exclude proxies matching a pattern for the provider inside the targeted container, or show current exclusions with no pattern.", "  urnet-docker proxy exclude 1.2.3.4 --unit mynetwork-provider\n"+
			"  urnet-docker proxy exclude --unit mynetwork-provider"),
	)
	return proxy
}

func newDockerSelfHealCmd() *cobra.Command {
	return withHelp(newCobraCmd("self-heal", "manage automatic proxy self-healing", []string{"selfheal"}, func(cmd *cobra.Command, args []string) error {
		return cmdDockerSelfHeal(args)
	}), "Toggle or report the self-heal marker inside the targeted container, delegating to its urnet-tools self-heal on|off|status.", "  urnet-docker self-heal status --unit mynetwork-provider\n  urnet-docker self-heal on --unit mynetwork-provider")
}

func newDockerSetCmd() *cobra.Command {
	return withHelp(newCobraCmd("set", "runtime tuning override in container state", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerSet(args)
	}), "Read or write a runtime tuning override in the container's provider state, delegating to its urnet-tools set. Run with no key to list, a key alone to show a value, a key and value to set it, or a key and \"off\" to clear it.", "  urnet-docker set report-interval 5m --unit mynetwork-provider\n  urnet-docker set cleanup-scope off --unit mynetwork-provider")
}

func newDockerFastAuthCmd() *cobra.Command {
	return withHelp(newCobraCmd("fast-auth", "manage auth rate limiter bypass marker", []string{"fastauth"}, func(cmd *cobra.Command, args []string) error {
		return cmdDockerFastAuth(args)
	}), "Manage the auth rate limiter bypass marker inside the targeted container, delegating to its urnet-tools fast-auth on|off|status.", "  urnet-docker fast-auth status --unit mynetwork-provider\n  urnet-docker fast-auth on --unit mynetwork-provider")
}

func newDockerHubCmd() *cobra.Command {
	return withHelp(newCobraCmd("hub", "delegate hub management commands", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerHub(args)
	}), "Manage the hub for the provider inside the targeted container. set and off point its reporting at a URL or remove it; install and init provision the hub service and its TLS certificate authority; link and unlink trust or untrust a hub; test verifies TLS connectivity; onboard-cmd mints a short-lived onboard token; show-password prints the CA password; update reinstalls the hub binary; open-port opens a firewall port for it. Delegates to the container's own urnet-tools hub.", "  urnet-docker hub set http://192.0.2.10:8080 --unit mynetwork-provider\n  urnet-docker hub link https://hub.example.com:8443 --unit mynetwork-provider\n  urnet-docker hub off --unit mynetwork-provider")
}

func newDockerSessionCmd() *cobra.Command {
	return withHelp(newCobraCmd("session", "export/import encrypted identity+proxy bundle", nil, func(cmd *cobra.Command, args []string) error {
		return cmdDockerSession(args)
	}), "Export or import the identity and proxy state of the provider inside the targeted container as an encrypted bundle, delegating to its urnet-tools session. 'session save' prompts for a passphrase inside the container's interactive session and writes the output file inside the container's filesystem, so copy it out with docker cp if you need it on the host. 'session load' backs up current state first and requires the bundle file to already exist inside the container.", "  urnet-docker session save /tmp/urnet-session.enc --unit mynetwork-provider\n  urnet-docker session load /tmp/urnet-session.enc --unit mynetwork-provider")
}

func newDockerExecCmd() *cobra.Command {
	// MUST NOT use newCobraCmd: its broad hasHelpFlag intercepts '--help' AFTER
	// the '--' separator, which belongs to the container command being run
	// splitExecArgs decides
	// what is help; delegate straight through.
	return &cobra.Command{
		Use:                "exec [target] [--] <cmd...>",
		Short:              "run arbitrary command inside container",
		Long:               "Run an arbitrary command inside the targeted container. Target flags (--unit/--network/--network-id/--state-dir) must come before the command; use a \"--\" separator so the container command's own flags are never mistaken for urnet-docker's.",
		Example:            "  urnet-docker exec --unit mynetwork-provider -- urnet-tools proxy add --proxy_file=/tmp/proxies.txt\n  urnet-docker exec mynetwork-provider -- ls /root/.urnetwork",
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
