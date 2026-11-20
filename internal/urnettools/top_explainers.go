package urnettools

import (
	"strings"

	"github.com/urnetwork/connect/internal/tui"
)

// topExplainer defines one help explainer entry displayed in a hover or touch
// tooltip on the top screen.
type topExplainer struct {
	Region string   // primary hit region name or canonical identifier
	Title  string   // concise box title
	Lines  []string // one to three short plain sentences explaining the component
}

// topExplainerCatalog contains the canonical registry of all top screen explainers.
var topExplainerCatalog = []topExplainer{
	// Sessions and clients (Now panel)
	{
		Region: "now:sessions",
		Title:  "Client Sessions",
		Lines: []string{
			"Active client sessions connected through this node.",
			"Shows post-quantum encrypted (pqe) and classical (cl) split.",
		},
	},
	{
		Region: "pqe",
		Title:  "PQE Sessions",
		Lines: []string{
			"Post-Quantum Encrypted client sessions.",
			"Uses hybrid ML-KEM and Kyber key exchange algorithms.",
		},
	},
	{
		Region: "cl",
		Title:  "Classical Sessions",
		Lines: []string{
			"Client sessions using classical cryptographic algorithms.",
			"Includes traditional key exchange and unencrypted fallback sessions.",
		},
	},
	{
		Region: "clients",
		Title:  "Clients",
		Lines: []string{
			"Count of distinct client endpoints currently routing traffic.",
			"Each client may maintain one or more concurrent transport sessions.",
		},
	},
	{
		Region: "now:pressure",
		Title:  "Memory Pressure",
		Lines: []string{
			"Host memory and scheduler pressure score from 0.00 to 1.00.",
			"High pressure above 0.70 triggers autopilot protective trims.",
		},
	},
	{
		Region: "now:billable",
		Title:  "Billable Bandwidth",
		Lines: []string{
			"Current rate of client data that earns network rewards.",
			"Excludes internal management overhead, proxy checks, and health probes.",
		},
	},
	{
		Region: "now:total",
		Title:  "Total Bandwidth",
		Lines: []string{
			"Total current network transfer rate across all node interfaces.",
			"Includes billable client traffic, health checks, gossip, and retransmits.",
		},
	},
	{
		Region: "now:avg1m",
		Title:  "1-Minute Average",
		Lines: []string{
			"Rolling 1-minute moving average of billable network transfer rate.",
		},
	},
	{
		Region: "now:avg5m",
		Title:  "5-Minute Average",
		Lines: []string{
			"Rolling 5-minute moving average of billable network transfer rate.",
		},
	},
	{
		Region: "now:total_avg1m",
		Title:  "1-Minute Total Average",
		Lines: []string{
			"Rolling 1-minute moving average of total network transfer rate.",
		},
	},
	{
		Region: "now:total_avg5m",
		Title:  "5-Minute Total Average",
		Lines: []string{
			"Rolling 5-minute moving average of total network transfer rate.",
		},
	},
	{
		Region: "now:billed",
		Title:  "Cumulative Billed",
		Lines: []string{
			"Total billable client volume transferred since provider startup.",
		},
	},
	{
		Region: "now:moved",
		Title:  "Cumulative Moved",
		Lines: []string{
			"Total raw byte volume transferred across all paths since startup.",
		},
	},
	{
		Region: "now:restart_pending",
		Title:  "Restart Pending",
		Lines: []string{
			"A configuration update or binary hotswap is scheduled.",
			"The provider will restart when active client sessions drain.",
		},
	},

	// Proxy tooltip age and states
	{
		Region: "age",
		Title:  "Clients Attached For",
		Lines: []string{
			"Continuous time clients have been attached to this proxy.",
			"Resets to idle when no clients have been connected for 10 seconds.",
			"Idle means no active clients right now, not a broken proxy.",
		},
	},
	{
		Region: "up",
		Title:  "Up State",
		Lines: []string{
			"Proxy is authenticated, passing health checks, and serving traffic.",
			"Actively accepting incoming connections from the client pool.",
		},
	},
	{
		Region: "dropped",
		Title:  "Dropped State",
		Lines: []string{
			"Proxy experienced transient network errors or packet loss.",
			"Temporarily degraded while health checks verify connectivity.",
		},
	},
	{
		Region: "connecting",
		Title:  "Connecting State",
		Lines: []string{
			"Proxy is establishing connection and authenticating credentials.",
			"Transitions to up once the upstream handshake succeeds.",
		},
	},
	{
		Region: "dead",
		Title:  "Dead State",
		Lines: []string{
			"Proxy failed health verification or authentication repeatedly.",
			"Excluded from routing until the next periodic retry cycle.",
		},
	},
	{
		Region: "parked",
		Title:  "Parked State",
		Lines: []string{
			"Proxy is temporarily idle under autopilot capacity controls.",
			"Conserves host memory while staying ready for fast unparking.",
		},
	},

	// UDP coverage states
	{
		Region: "udp:ok",
		Title:  "UDP OK",
		Lines: []string{
			"Proxy passed UDP ASSOCIATE test with verified relay delivery.",
			"Outbound UDP packets match the proxy TCP public IP address.",
		},
	},
	{
		Region: "udp:no_relay",
		Title:  "No UDP",
		Lines: []string{
			"Proxy refused UDP ASSOCIATE or relay endpoint did not answer.",
			"Only TCP client traffic is routed through this proxy.",
		},
	},
	{
		Region: "udp:mismatch",
		Title:  "UDP Mismatch",
		Lines: []string{
			"UDP relay succeeded but emerged from a different public IP than TCP.",
			"Flagged for routing transparency and protocol consistency.",
		},
	},
	{
		Region: "udp:unknown",
		Title:  "UDP Unknown",
		Lines: []string{
			"UDP relay test answered but TCP exit address could not be verified.",
			"Egress parity could not be determined in this check cycle.",
		},
	},
	{
		Region: "udp:native",
		Title:  "Native UDP",
		Lines: []string{
			"Direct network interface handles UDP without proxy encapsulation.",
			"Routes packets through host default gateway with native performance.",
		},
	},
	{
		Region: "udp:unchecked",
		Title:  "UDP Unchecked",
		Lines: []string{
			"Proxy is online but background probe has not completed yet.",
			"UDP sweeps run periodically in background to verify support.",
		},
	},
	{
		Region: "udp:coverage",
		Title:  "UDP Coverage",
		Lines: []string{
			"Percentage of online proxies verified for UDP relay capability.",
			"Shows checked ratio and total online pool coverage.",
		},
	},

	// Resources panel
	{
		Region: "res:heap",
		Title:  "Heap Memory",
		Lines: []string{
			"Go runtime heap memory in use against the configured memory cap.",
			"Tracked by autopilot to prevent process out-of-memory termination.",
		},
	},
	{
		Region: "res:fds",
		Title:  "File Descriptors",
		Lines: []string{
			"Open file descriptors against host operating system process limit.",
			"Each active proxy connection and client socket consumes a descriptor.",
		},
	},
	{
		Region: "res:rss",
		Title:  "Resident Memory (RSS)",
		Lines: []string{
			"Physical RAM occupied by the provider process in host memory.",
			"Includes Go runtime heap, execution stacks, and internal buffers.",
		},
	},
	{
		Region: "swap",
		Title:  "Swap Usage",
		Lines: []string{
			"Host swap space utilized by provider process or operating system.",
			"Non-zero swap indicates host physical memory limits are saturated.",
		},
	},
	{
		Region: "gor",
		Title:  "Goroutines",
		Lines: []string{
			"Active Go goroutines executing inside the provider runtime.",
			"Press g to open the detailed goroutine stack inspection panel.",
		},
	},

	// Internals panel
	{
		Region: "internals:objects",
		Title:  "Heap Objects",
		Lines: []string{
			"Total allocated live heap objects managed by the Go runtime.",
		},
	},
	{
		Region: "internals:stacks",
		Title:  "Stack Memory",
		Lines: []string{
			"Memory allocated for goroutine execution stacks.",
		},
	},
	{
		Region: "internals:goal",
		Title:  "GC Goal",
		Lines: []string{
			"Target heap size for the next garbage collection cycle.",
		},
	},
	{
		Region: "internals:alloc",
		Title:  "Allocation Rate",
		Lines: []string{
			"Byte rate of new memory allocations per second by the provider.",
		},
	},
	{
		Region: "internals:gc",
		Title:  "GC Cycles",
		Lines: []string{
			"Garbage collection frequency per minute and 99th percentile pause time.",
		},
	},
	{
		Region: "internals:gccpu",
		Title:  "GC CPU Fraction",
		Lines: []string{
			"Fraction of total available CPU time consumed by garbage collection.",
		},
	},
	{
		Region: "internals:sched",
		Title:  "Scheduler Latency",
		Lines: []string{
			"99th percentile time runnable goroutines wait for available OS threads.",
		},
	},
	{
		Region: "h1",
		Title:  "HTTP/1.1 Transport",
		Lines: []string{
			"Standard TCP-based HTTP connection transport fallback.",
			"Used when UDP, QUIC, or HTTP/3 paths are blocked by network firewalls.",
		},
	},
	{
		Region: "h3",
		Title:  "HTTP/3 Transport",
		Lines: []string{
			"High-performance QUIC transport running over UDP datagrams.",
			"Provides multiplexed streams and zero round-trip connection restarts.",
		},
	},
	{
		Region: "internals:h3all",
		Title:  "H3 Outbound Share",
		Lines: []string{
			"Cumulative outbound bytes transferred over HTTP/3 versus total payload.",
		},
	},

	// Extender & Gossip mesh
	{
		Region: "extender:state",
		Title:  "Extender Role",
		Lines: []string{
			"Status of the local extender relay role and registration.",
			"Extenders announce their availability to clients through the gossip mesh.",
		},
	},
	{
		Region: "extender:gossip",
		Title:  "Gossip Mesh",
		Lines: []string{
			"Gossip carries the directory of working extenders, not client traffic.",
			"Nodes exchange membership announcements to maintain network topology.",
		},
	},
	{
		Region: "gossip:peers",
		Title:  "Gossip Peers",
		Lines: []string{
			"Number of other nodes linked to this extender in the gossip mesh.",
			"Peers exchange signed extender directory updates across the network.",
		},
	},
	{
		Region: "gossip:feeds",
		Title:  "Directory Feeds",
		Lines: []string{
			"Number of clients holding an open subscription to this directory feed.",
			"Feeds 0 means no client is subscribed right now, not that nobody uses it.",
		},
	},
	{
		Region: "gossip:msgs",
		Title:  "Gossip Messages",
		Lines: []string{
			"Number of valid directory messages received from peers since start.",
			"Gossip carries the directory of working extenders, not client traffic.",
		},
	},
	{
		Region: "extender:clients",
		Title:  "Extender Clients",
		Lines: []string{
			"Number of client connections currently routing through this extender.",
		},
	},
	{
		Region: "carriers",
		Title:  "Extender Carriers",
		Lines: []string{
			"Transport carriers listening for client traffic on this extender.",
			"Shows bytes transferred, active connections, and refusal diagnostics.",
		},
	},
	{
		Region: "extender:served",
		Title:  "Extender Served History",
		Lines: []string{
			"Unique clients served by this extender over 24 hours, 7 days, and lifetime.",
		},
	},
	{
		Region: "extender:ports",
		Title:  "Extender Port Bindings",
		Lines: []string{
			"Network port listeners bound for incoming extender client traffic.",
		},
	},

	// Heatmap styles
	{
		Region: "glow",
		Title:  "Glow Heatmap",
		Lines: []string{
			"Default half-block proxy grid with afterglow decay.",
			"Brightens cells based on real-time throughput intensity.",
		},
	},
	{
		Region: "aurora",
		Title:  "Aurora Heatmap",
		Lines: []string{
			"Flowing color wave visualizer showing relative proxy bandwidth.",
			"Smoothly transitions hues across active and recovering proxies.",
		},
	},
	{
		Region: "sonar",
		Title:  "Sonar Heatmap",
		Lines: []string{
			"Periodic radial scan highlighting background health probe sweeps.",
			"Visualizes check completion order across configured proxies.",
		},
	},
	{
		Region: "locality",
		Title:  "Locality Heatmap",
		Lines: []string{
			"Groups proxies into subnet blocks and network provider clusters.",
			"Displays diversity and concentration across egress routes.",
		},
	},
	{
		Region: "blocks",
		Title:  "Blocks Heatmap",
		Lines: []string{
			"Discrete block cells showing high-contrast proxy state characters.",
			"Avoids half-block packing for maximum readability on small fonts.",
		},
	},
	{
		Region: "timeline",
		Title:  "Timeline Heatmap",
		Lines: []string{
			"Chronological activity bars tracking client session distribution.",
			"Highlights recently active proxies against idle pool capacity.",
		},
	},
	{
		Region: "scan",
		Title:  "Scan Heatmap",
		Lines: []string{
			"Real-time UDP sweep scan wave across configured proxies.",
			"Flashes check results in real time with progressive afterglow fade.",
		},
	},

	// Legend entries
	{
		Region: "legend:direct",
		Title:  "Direct Path",
		Lines: []string{
			"Local host network interface (proxy slot 0).",
			"Routes client traffic directly through host egress without a proxy.",
		},
	},
	{
		Region: "legend:up",
		Title:  "Up Count",
		Lines: []string{
			"Count of healthy, verified proxies ready for client traffic.",
		},
	},
	{
		Region: "legend:dropped",
		Title:  "Dropped Count",
		Lines: []string{
			"Count of degraded proxies recovering from dropped connections.",
		},
	},
	{
		Region: "legend:connecting",
		Title:  "Connecting Count",
		Lines: []string{
			"Count of proxies actively establishing upstream connections.",
		},
	},
	{
		Region: "legend:dead",
		Title:  "Dead Count",
		Lines: []string{
			"Count of unreachable proxies removed from active rotation.",
		},
	},
	{
		Region: "legend:parked",
		Title:  "Parked Count",
		Lines: []string{
			"Count of idle proxies parked by autopilot to conserve host RAM.",
		},
	},
	{
		Region: "legend:style",
		Title:  "Heatmap Style Selector",
		Lines: []string{
			"Current proxy heatmap visualization style.",
			"Change via the m settings menu to customize visual appearance.",
		},
	},

	// Graphs
	{
		Region: "box:graph",
		Title:  "Throughput Graph",
		Lines: []string{
			"Real-time bandwidth graph showing transfer history.",
			"Press s to split upload and download, and press w to toggle vertical zoom.",
		},
	},
	{
		Region: "title:graph",
		Title:  "Graph Mode & Scale",
		Lines: []string{
			"Graph title showing active history window and vertical peak scale.",
			"Click or press w to toggle zoom, and press t to cycle time intervals.",
		},
	},

	// Panels & boxes
	{
		Region: "box:proxies",
		Title:  "Proxy Pool Panel",
		Lines: []string{
			"Visual heatmap and status distribution of configured proxies.",
			"Hover individual dots for address, state, and traffic details.",
		},
	},
	{
		Region: "title:proxies",
		Title:  "Proxies Header",
		Lines: []string{
			"Proxy pool management panel header.",
			"Displays real-time cell matrix and background UDP probe telemetry.",
		},
	},
	{
		Region: "box:events",
		Title:  "Events Panel",
		Lines: []string{
			"Chronological log of recent node events, transitions, and errors.",
			"Scroll with mouse wheel or Up and Down arrow keys.",
		},
	},
	{
		Region: "title:events",
		Title:  "Events Header",
		Lines: []string{
			"Provider diagnostic event log header.",
		},
	},
	{
		Region: "box:now",
		Title:  "Now Panel",
		Lines: []string{
			"Real-time summary of bandwidth rates, client sessions, and pressure.",
			"Displays active performance state and operational diagnostics.",
		},
	},
	{
		Region: "title:now",
		Title:  "Now Header",
		Lines: []string{
			"Current node telemetry and status overview.",
		},
	},
	{
		Region: "box:resources",
		Title:  "Resources Panel",
		Lines: []string{
			"Operating system and Go runtime resource utilization.",
			"Monitors memory heap, file descriptors, and physical resident memory.",
		},
	},
	{
		Region: "title:resources",
		Title:  "Resources Header",
		Lines: []string{
			"Process resource consumption metrics and limits.",
		},
	},
	{
		Region: "box:extender",
		Title:  "Extender Panel",
		Lines: []string{
			"Extender relay status, port listeners, carriers, and gossip mesh.",
			"Shown when the provider operates in relay extender mode.",
		},
	},
	{
		Region: "title:extender",
		Title:  "Extender Header",
		Lines: []string{
			"Extender service and mesh directory status.",
		},
	},
	{
		Region: "box:internals",
		Title:  "Internals Panel",
		Lines: []string{
			"Go runtime internals: memory allocation, GC pacing, and QUIC/H3 stats.",
			"Press g to view active goroutine stack groups.",
		},
	},
	{
		Region: "title:internals",
		Title:  "Internals Header",
		Lines: []string{
			"Go runtime engine performance metrics.",
		},
	},

	// Dividers
	{
		Region: "side",
		Title:  "Side Column Divider",
		Lines: []string{
			"Vertical boundary between main charts and sidebar panels.",
			"Click and drag to resize width, or double-click to reset default.",
		},
	},
	{
		Region: "left:proxies",
		Title:  "Proxies Divider",
		Lines: []string{
			"Horizontal boundary between throughput graph and proxy pool.",
			"Click and drag to adjust height, or double-click to reset default.",
		},
	},
	{
		Region: "left:events",
		Title:  "Events Divider",
		Lines: []string{
			"Horizontal boundary between proxy pool and events log.",
			"Click and drag to adjust height, or double-click to reset default.",
		},
	},
	{
		Region: "side:resources",
		Title:  "Resources Divider",
		Lines: []string{
			"Horizontal boundary between now and resources panels.",
			"Click and drag to adjust height, or double-click to reset default.",
		},
	},
	{
		Region: "side:extender",
		Title:  "Extender Divider",
		Lines: []string{
			"Horizontal boundary between resources and extender panels.",
			"Click and drag to adjust height, or double-click to reset default.",
		},
	},
	{
		Region: "side:internals",
		Title:  "Internals Divider",
		Lines: []string{
			"Horizontal boundary between extender and internals panels.",
			"Click and drag to adjust height, or double-click to reset default.",
		},
	},

	// Header and tab
	{
		Region: "tab:provider",
		Title:  "Provider Selector",
		Lines: []string{
			"Current provider instance, software version, and uptime duration.",
			"Click or press Tab to cycle between running providers on this host.",
		},
	},

	// Footer keys
	{
		Region: "footer:quit",
		Title:  "Quit Shortcut",
		Lines: []string{
			"Exit urnet-tools top and restore the terminal display.",
			"Keyboard shortcut: q or Ctrl+C.",
		},
	},
	{
		Region: "footer:provider",
		Title:  "Provider Switch",
		Lines: []string{
			"Cycle display to the next running provider found on this host.",
			"Keyboard shortcut: Tab.",
		},
	},
	{
		Region: "footer:help",
		Title:  "Help Overlay",
		Lines: []string{
			"Open overlay listing all navigation keys and interactive commands.",
			"Keyboard shortcut: ? (question mark).",
		},
	},
	{
		Region: "footer:menu",
		Title:  "Settings Menu",
		Lines: []string{
			"Open menu to configure heatmap styles, themes, and unit formatting.",
			"Keyboard shortcut: m.",
		},
	},
	{
		Region: "footer:zoom",
		Title:  "Graph Zoom",
		Lines: []string{
			"Toggle vertical graph scaling between auto-range and peak zoom.",
			"Keyboard shortcut: w.",
		},
	},
	{
		Region: "footer:proxies",
		Title:  "Proxy Heatmap Window",
		Lines: []string{
			"Open near full-screen proxy heatmap window for fleet inspection.",
			"Keyboard shortcut: p.",
		},
	},
	{
		Region: "footer:goroutines",
		Title:  "Goroutines Panel",
		Lines: []string{
			"Open live stack view grouping all active runtime goroutines.",
			"Keyboard shortcut: g.",
		},
	},
	{
		Region: "footer:interval",
		Title:  "Refresh Interval",
		Lines: []string{
			"Adjust dashboard polling and redraw frequency from 100ms to 10s.",
			"Click - half to poll faster, and click + half to poll slower.",
		},
	},
	{
		Region: "footer:mouse",
		Title:  "Mouse Reporting",
		Lines: []string{
			"Toggle terminal mouse event capture and click interactions.",
			"Keyboard shortcut: M (Shift+m).",
		},
	},
}

// topExplainerMap caches lookup entries keyed by primary region and aliases.
var topExplainerMap map[string]topExplainer

func init() {
	topExplainerMap = make(map[string]topExplainer, len(topExplainerCatalog)*3)
	for _, exp := range topExplainerCatalog {
		topExplainerMap[exp.Region] = exp
	}

	// Register canonical aliases so lookups by raw metric names or prefixes resolve cleanly
	aliases := map[string]string{
		// Sessions & Clients
		"now:pqe":                "pqe",
		"now:cl":                 "cl",
		"now:clients":            "clients",
		"now:total":              "now:total",
		"billable":               "now:billable",
		"total":                  "now:total",
		"pressure":               "now:pressure",
		"clients_attached_for":   "age",
		"proxy:age":              "age",
		"proxy:clients_attached": "age",

		// Proxy states
		"state:up":         "up",
		"state:dropped":    "dropped",
		"state:connecting": "connecting",
		"state:dead":       "dead",
		"state:parked":     "parked",

		// UDP states
		"no UDP":            "udp:no_relay",
		"no-udp":            "udp:no_relay",
		"udp:no UDP":        "udp:no_relay",
		"udp:no-udp":        "udp:no_relay",
		"mismatch":          "udp:mismatch",
		"diff":              "udp:mismatch",
		"unknown":           "udp:unknown",
		"unk":               "udp:unknown",
		"native":            "udp:native",
		"native (no relay)": "udp:native",
		"unchecked":         "udp:unchecked",

		// Resources
		"heap":     "res:heap",
		"fds":      "res:fds",
		"rss":      "res:rss",
		"res:swap": "swap",
		"res:gor":  "gor",

		// Internals
		"internals:gor": "gor",
		"internals:h3":  "h3",
		"internals:h1":  "h1",
		"h3/all":        "internals:h3all",

		// Extender & Gossip
		"peers":             "gossip:peers",
		"feeds":             "gossip:feeds",
		"msgs":              "gossip:msgs",
		"gossip":            "extender:gossip",
		"extender:peers":    "gossip:peers",
		"extender:feeds":    "gossip:feeds",
		"extender:msgs":     "gossip:msgs",
		"extender:carriers": "carriers",

		// Heat styles
		"heat:glow":     "glow",
		"heat:aurora":   "aurora",
		"heat:sonar":    "sonar",
		"heat:locality": "locality",
		"heat:blocks":   "blocks",
		"heat:timeline": "timeline",
		"heat:scan":     "scan",
		"Glow":          "glow",
		"Aurora":        "aurora",
		"Sonar":         "sonar",
		"Locality":      "locality",
		"Blocks":        "blocks",
		"Timeline":      "timeline",
		"Scan":          "scan",

		// Graph
		"graph:billable": "box:graph",
		"graph:total":    "box:graph",
	}

	for alias, target := range aliases {
		if exp, ok := topExplainerMap[target]; ok {
			topExplainerMap[alias] = exp
		}
	}
}

// topExplainerFor resolves an explainer by region name or alias.
func topExplainerFor(name string) (topExplainer, bool) {
	if name == "" {
		return topExplainer{}, false
	}
	if exp, ok := topExplainerMap[name]; ok {
		return exp, true
	}
	// Fallback alias resolution for prefixed names
	if after, ok := strings.CutPrefix(name, "legend:"); ok {
		if exp, ok := topExplainerMap[after]; ok {
			return exp, true
		}
	}
	if after, ok := strings.CutPrefix(name, "state:"); ok {
		if exp, ok := topExplainerMap[after]; ok {
			return exp, true
		}
	}
	if after, ok := strings.CutPrefix(name, "heat:"); ok {
		if exp, ok := topExplainerMap[strings.ToLower(after)]; ok {
			return exp, true
		}
	}
	if after, ok := strings.CutPrefix(name, "title:"); ok {
		if exp, ok := topExplainerMap["box:"+after]; ok {
			return exp, true
		}
	}
	return topExplainer{}, false
}

// allTopExplainers returns the canonical catalog of explainers for documentation
// and verification tests.
func allTopExplainers() []topExplainer {
	out := make([]topExplainer, len(topExplainerCatalog))
	copy(out, topExplainerCatalog)
	return out
}

// drawExplainerPopup renders the active explainer tooltip box over the top buffer.
func (m *topModel) drawExplainerPopup(b *tui.Buffer) {
	exp, ok := topExplainerFor(m.explainerName)
	if !ok || len(exp.Lines) == 0 {
		return
	}
	rect := b.Rect()
	if rect.W < 4 || rect.H < 4 {
		return
	}
	maxLine := tui.StringWidth(exp.Title)
	for _, l := range exp.Lines {
		maxLine = max(maxLine, tui.StringWidth(l))
	}
	// Determine box dimensions, bounded by buffer size
	w := min(rect.W, max(maxLine+4, 24))
	h := min(rect.H, len(exp.Lines)+2)

	// Position near cursor (x+1, y+1), clamped within buffer rectangle
	x := min(max(m.explainerX+1, rect.X), rect.X+rect.W-w)
	y := min(max(m.explainerY+1, rect.Y), rect.Y+rect.H-h)

	panel := b.Sub(tui.Rect{X: x, Y: y, W: w, H: h})
	panel.Fill(panel.Rect(), ' ', tui.Style{})
	in := tui.DrawBox(panel, exp.Title, m.theme.Frame, m.theme.Border, m.theme.Accent, m.theme.ASCII)
	for i, line := range exp.Lines {
		if i >= in.Height() {
			break
		}
		in.Put(tui.Truncate(line, in.Width(), m.theme.ASCII), 0, i, tui.Style{})
	}
}
