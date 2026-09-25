# `urnet-tools top`: a live terminal view of a provider (design proposal)

> [!NOTE]
> **Status: implemented, not yet released.** Built on top of the [live status snapshot](live-status-snapshot.md). The design below is kept as written, with the differences from what was built listed under [Implementation notes](#implementation-notes).

## Problem

There is no live view of a provider. `urnet-tools status` on Linux prints `systemctl status`, `dashboard` is a one-shot panel, and `proxy traffic` shows real-time bandwidth and session load for proxies but nothing about the node as a whole. Answering "what is this box doing right now" means combining several commands, or reading logs.

## Goal

`urnet-tools top`, also reachable as `urtop`, is a full-screen, keyboard-driven view in the spirit of btop: live graphs, dense panels, one screen, no scrolling log. It reads only the provider's control socket, so it works with `/metrics` switched off and never needs Grafana.

## Non-goals

- It does not change any setting or restart anything. Read-only in the first version.
- It is not a log viewer. `logs` remains the tool for that.
- It does not replace `status` (scriptable, one-shot) or `dashboard` (settings and warnings).
- Windows is not a first-version target (see [Open questions](#open-questions)).

## Layout

One provider per screen, sized to the terminal. Illustration only:

```
 urnet-tools top   node: tornado   v3.23.0-fix.32.1   up 3d 4h        FLOWING    12:04:31
┌ Throughput ─────────────────────────────────────────┐┌ Now ──────────────────────┐
│ 41 MiB/s ┤        ⣀⣠⣴⣾⣿⣷⣦⣄⣀    ⣀⣤⣶⣿⣿⣿                ││ billable   38.2 MiB/s      │
│          ┤   ⣀⣤⣶⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣷⣶⣶⣿⣿⣿⣿⣿⣿⣿⣷⣄             ││ 1m avg     41.0 MiB/s      │
│  0       ┼──────────────────────────────── 10 min ││ 5m avg     35.4 MiB/s      │
└─────────────────────────────────────────────────────┘│ clients    212             │
┌ Proxies ────────────────────────────────────────────┐│ pressure   ▂ 0.21          │
│ up ██████████████████████████████████  58           │└────────────────────────────┘
│ degraded ██  2      connecting  0      dead  0      │┌ Resources ─────────────────┐
│ top by billable/s   proxy-a  4.1 MiB/s   proxy-b ...││ heap  ███████░░░ 1.8/4.0 GiB│
└─────────────────────────────────────────────────────┘│ fds   ▏ 310 / 65536        │
┌ Events ─────────────────────────────────────────────┐│ gor   1204                 │
│ 12:03 proxy-c recovered   12:01 restart: update ...  │└────────────────────────────┘
└─────────────────────────────────────────────────────┘
 q quit   tab provider   p proxies   r refresh rate   ? help
```

Panels:

- **Throughput:** the last 10 minutes of billable rate as a braille graph, from the snapshot's sample ring, redrawn once per second.
- **Now:** current, 1 minute and 5 minute rate, clients, pressure, and the state verdict with the "why is it idle" hint when idle.
- **Proxies:** pool by status and the busiest proxies by billable rate.
- **Resources:** heap against the memory limit, descriptors against their limit, goroutines.
- **Events:** recent restarts (with reason) and proxy recoveries and losses, from data the provider already keeps.

Keys: `q` quit, `tab` next provider on the host, `p` proxy detail, `?` help, `-`/`+` faster or slower refresh (as in btop), `w` zoom, `g` goroutine list, `m` menu. Resize is handled live.

## Data flow

`top` asks the control socket for `snapshot` on a timer (default 1 s). The snapshot is cached in the provider for about a second, so several open `top` sessions plus a scrape cost roughly one computation per second. `top` keeps no history of its own beyond the ring it receives, so starting it mid-run shows the last 10 minutes immediately.

If the provider stops answering, the screen stays up, shows `DISCONNECTED` with a countdown, and resumes on its own when the socket returns (for example across a restart or hotswap).

## The library decision

`go.mod` today has `golang.org/x/term` and `golang.org/x/sys` and no TUI framework.

| Option | For | Against |
|---|---|---|
| **Small internal renderer on `x/term`** (recommended) | No new dependency in a production binary that ships to a fleet. Fully deterministic: rendering to a cell buffer means the whole screen can be golden-file tested with no terminal. We control exactly what escape sequences are sent. | We write and maintain the cell buffer, the diffing writer, box drawing and the braille graph, a few hundred lines. |
| A framework (for example bubbletea with lipgloss) | Faster to a polished result, mature input and layout handling. | Adds a dependency tree to every build target, and its update cadence becomes ours to track. Tests are harder to make deterministic. |

Recommendation: the internal renderer, as `internal/tui`, with the widgets `top` needs (box, sparkline, bar, braille graph, table) and nothing more. The same widgets can later back the `status` sparkline so there is one implementation.

**As built:** the widgets, cell buffer, frame diffing and layout are the internal `internal/tui`, as recommended. The one difference is terminal I/O: `internal/tui/tcellui` uses `tcell` for raw mode, the alternate screen, key decoding and resize instead of `x/term`. That adds one dependency (`tcell`, Apache-2.0) and three small indirect ones (`gdamore/encoding` Apache-2.0, `go-colorful` MIT, `uniseg` MIT), all permissive.

## Design notes

- **Rendering:** an off-screen cell buffer, diffed against the previous frame so only changed cells are written. Synchronised-update sequences are used where the terminal supports them, to avoid tearing.
- **Terminal handling:** raw mode through `x/term`, alternate screen, and cursor restored on every exit path including panic and `SIGTERM`. A terminal left in raw mode is the classic TUI failure, so restore is tested explicitly.
- **Colour:** truecolor where advertised, then 256, then 16, then none. `NO_COLOR` and `TERM=dumb` give a plain layout that draws the graph and bars in ASCII characters instead of braille and colour.
- **Too small:** below a minimum size, a single-panel compact view instead of a broken layout.
- **`urtop`:** a third symlink beside the existing `urnet-tools` and `urnetwork` links (`link_tools_into_dir` in the installer); the tool checks its own executable name and behaves as `top`. `urnet-tools update` repairs the link on older installs, as it already does for the PATH links.
- **Multiple providers and containers:** `tab` cycles through what `Discover()` finds. Container providers reuse the same path `urnet-docker` already uses to reach a container's socket.

## Testing

- **Golden frames:** render fixed snapshots at a few terminal sizes and diff the text, including the empty, idle, degraded and disconnected states.
- **Widget tests:** sparkline and braille graph scaling, bar clamping, table truncation with wide characters.
- **Input:** key handling driven from a scripted reader, including resize during redraw.
- **Terminal restore:** a test that forces a panic and a signal and asserts the restore sequence was written.
- **Snapshot source:** a fake control socket that serves scripted snapshots, including one that stops answering.

## Rollout

After the snapshot work lands in `urnetwork-3.23-fix`, then ported to `meso-miner` and `sn` the same way. Ships as an ordinary point release, not a release candidate.

## Open questions

- **Windows.** The Windows console needs virtual-terminal processing enabled explicitly. `x/sys` is already a dependency so it is possible, but the first version is Linux and macOS; Windows keeps `status` and `dashboard`.
- **Read-only or actions.** Whether a later version should offer actions (trim proxies, refresh) from inside `top`. First version: no.
- **`proxy traffic`.** Whether it becomes a `top` view and is retired, or stays as a scriptable command. Leaning: stays, and `top` links to the same data.

## Implementation notes

What was built differs from the proposal in these ways:

- **Terminal I/O** goes through `tcell`, not `x/term` (see [The library decision](#the-library-decision)).
- **Keys:** `q`, `Esc` and `Ctrl-C` quit; `Tab` and `Shift-Tab` switch provider; `-` and `+` shorten and lengthen the refresh interval, like btop; `w` zooms the graphs to the last 15 seconds; `g` lists where goroutines are parked; `m` opens the theme and graph style menu; `?` shows help. The proposed `p` (proxy detail) is **not** in the first version.
- **States:** a snapshot state of `starting`, or any state the view does not know, is shown in capitals in a neutral colour rather than as an error.
- **`--demo`:** a hidden flag that draws synthetic snapshots, computed purely from the clock, so the screen can be seen or captured (for example in `tmux`) on a box with no provider. It skips provider discovery and never touches real state. It is deliberately not in the help text.
- **`urtop`:** a link to the `urnet-tools` binary, created by the installer and by `urnet-tools update`; the binary behaves as `top` when started under that name.
- **Not a terminal:** with no interactive terminal on stdout it refuses before doing anything else and points at `urnet-tools status` and `--json`.
- **Windows** is still not a first-version target; it builds, but is untested there.

