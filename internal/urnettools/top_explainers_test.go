package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// TestEveryRegisteredRegionHasExplainer ensures every hit region recorded by topModel
// during a full frame has a matching explainer text in the registry.
func TestEveryRegisteredRegionHasExplainer(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(3), clock)
	m.apply(0, topFlowing(t), nil)

	// Enable UDP coverage data
	m.snap.Proxies.UDP = &SnapshotProxyUDP{
		OK:        10,
		NoRelay:   2,
		Mismatch:  1,
		Unknown:   1,
		Unchecked: 5,
	}

	// Enable Extender reading with gossip and carriers
	msgCount := uint64(1042)
	m.extender = ExtenderReading{
		Source:    SourceControlSocket,
		Known:     true,
		Setting:   "on",
		Running:   true,
		RoleState: "serving",
		Discovery: &ExtenderDiscovery{
			State:            "running",
			Peers:            6,
			FeedSubscribers:  3,
			MessagesAccepted: &msgCount,
		},
		Families: []ExtenderFamily{
			{IpVersion: 4, State: "activated", Ip: "192.0.2.1"},
			{IpVersion: 6, State: "activated", Ip: "2001:db8::1"},
		},
		Carriers: []ExtenderCarrierStats{
			{Carrier: "grpc", Served: 500, Refused: 2},
		},
	}

	// Enable runtime internals
	m.rt.ok = true
	m.rt.gOK = true
	m.rt.cur = &NodeInternals{
		Goroutines:       45,
		HeapObjectsBytes: 10 * 1024 * 1024,
		HeapStacksBytes:  2 * 1024 * 1024,
		HeapGoalBytes:    20 * 1024 * 1024,
		AllocBytes:       1024 * 1024,
		IntervalSeconds:  1,
		GCCycles:         12,
		GCPauseP99Ms:     1.5,
		GCCPUFraction:    0.02,
		SchedLatP99Ms:    2.0,
		Transport: &NodeTransportStats{
			H3Up:             1,
			H3TxSharePercent: 85,
			H3Drops:          0,
			H3BytesTx:        500 * 1024 * 1024,
			H1BytesTx:        50 * 1024 * 1024,
		},
	}

	b := tui.New(120, 40)
	m.render(b)

	if len(m.regions) == 0 {
		t.Fatal("no regions recorded in rendered frame")
	}

	for _, r := range m.regions {
		exp, ok := topExplainerFor(r.Name)
		if !ok {
			t.Errorf("region %q has no registered explainer", r.Name)
			continue
		}
		if exp.Title == "" {
			t.Errorf("region %q explainer has empty title", r.Name)
		}
		if len(exp.Lines) == 0 {
			t.Errorf("region %q explainer has no lines", r.Name)
		}
		for i, l := range exp.Lines {
			if strings.TrimSpace(l) == "" {
				t.Errorf("region %q explainer line %d is blank", r.Name, i)
			}
		}
	}
}

// TestTopExplainerRequiredTerms verifies that all terms required by the brief
// are present in the explainer registry with valid titles and descriptive sentences.
func TestTopExplainerRequiredTerms(t *testing.T) {
	required := []struct {
		term      string
		wantTitle string
		sub       string
	}{
		{"pqe", "PQE Sessions", "Post-Quantum Encrypted"},
		{"cl", "Classical Sessions", "classical"},
		{"clients", "Clients", "client endpoints"},
		{"age", "Clients Attached For", "Continuous time clients"},
		{"up", "Up State", "authenticated"},
		{"dropped", "Dropped State", "transient network errors"},
		{"connecting", "Connecting State", "establishing connection"},
		{"dead", "Dead State", "failed health verification"},
		{"parked", "Parked State", "autopilot capacity"},
		{"udp:ok", "UDP OK", "UDP ASSOCIATE"},
		{"udp:no_relay", "No UDP", "refused UDP ASSOCIATE"},
		{"udp:mismatch", "UDP Mismatch", "different public IP"},
		{"udp:unknown", "UDP Unknown", "could not be verified"},
		{"udp:native", "Native UDP", "Direct network interface"},
		{"udp:unchecked", "UDP Unchecked", "probe has not completed"},
		{"now:billable", "Billable Bandwidth", "earns network rewards"},
		{"now:total", "Total Bandwidth", "Total current network transfer rate"},
		{"gor", "Goroutines", "Active Go goroutines"},
		{"res:fds", "File Descriptors", "Open file descriptors"},
		{"res:rss", "Resident Memory (RSS)", "Physical RAM"},
		{"swap", "Swap Usage", "Host swap space"},
		{"now:pressure", "Memory Pressure", "pressure score"},
		{"carriers", "Extender Carriers", "Transport carriers"},
		{"h1", "HTTP/1.1 Transport", "TCP-based HTTP"},
		{"h3", "HTTP/3 Transport", "QUIC transport"},
		{"glow", "Glow Heatmap", "afterglow decay"},
		{"aurora", "Aurora Heatmap", "Flowing color wave"},
		{"sonar", "Sonar Heatmap", "Periodic radial scan"},
		{"locality", "Locality Heatmap", "subnet blocks"},
		{"blocks", "Blocks Heatmap", "Discrete block cells"},
		{"timeline", "Timeline Heatmap", "Chronological activity bars"},
		{"title:graph", "Graph Mode & Scale", "Graph title"},
		{"box:graph", "Throughput Graph", "Real-time bandwidth graph"},
		{"legend:direct", "Direct Path", "proxy slot 0"},
		{"legend:up", "Up Count", "Count of healthy"},
		{"legend:dropped", "Dropped Count", "Count of degraded"},
		{"legend:connecting", "Connecting Count", "Count of proxies actively"},
		{"legend:dead", "Dead Count", "Count of unreachable"},
		{"legend:parked", "Parked Count", "Count of idle"},
		{"footer:quit", "Quit Shortcut", "q or Ctrl+C"},
		{"footer:provider", "Provider Switch", "Tab"},
		{"footer:help", "Help Overlay", "navigation keys"},
		{"footer:menu", "Settings Menu", "heatmap styles"},
		{"footer:zoom", "Graph Zoom", "vertical graph scaling"},
		{"footer:goroutines", "Goroutines Panel", "runtime goroutines"},
		{"footer:interval", "Refresh Interval", "polling and redraw"},
		{"footer:mouse", "Mouse Reporting", "mouse event capture"},
		{"gossip:peers", "Gossip Peers", "Number of other nodes linked to this extender in the gossip mesh"},
		{"gossip:feeds", "Directory Feeds", "Number of clients holding an open subscription to this directory feed"},
		{"gossip:msgs", "Gossip Messages", "Number of valid directory messages received from peers since start"},
	}

	for _, tc := range required {
		exp, ok := topExplainerFor(tc.term)
		if !ok {
			t.Errorf("required term %q missing from explainer registry", tc.term)
			continue
		}
		if exp.Title != tc.wantTitle {
			t.Errorf("term %q title = %q, want %q", tc.term, exp.Title, tc.wantTitle)
		}
		body := strings.Join(exp.Lines, " ")
		if !strings.Contains(body, tc.sub) {
			t.Errorf("term %q body %q does not contain %q", tc.term, body, tc.sub)
		}
	}
}

// TestExplainerTooltipRendering tests popup rendering across wide, narrow, mono and ASCII themes.
func TestExplainerTooltipRendering(t *testing.T) {
	clock := &fakeClock{t: topBase}

	cases := []struct {
		name       string
		width      int
		height     int
		theme      tui.Theme
		region     string
		wantTitle  string
		wantBorder rune
	}{
		{
			name:       "WideDefault",
			width:      120,
			height:     40,
			theme:      tui.DefaultTheme(),
			region:     "gossip:feeds",
			wantTitle:  "Directory Feeds",
			wantBorder: '╭',
		},
		{
			name:       "NarrowDefault",
			width:      50,
			height:     20,
			theme:      tui.DefaultTheme(),
			region:     "now:sessions",
			wantTitle:  "Client Sessions",
			wantBorder: '╭',
		},
		{
			name:       "MonoTheme",
			width:      100,
			height:     30,
			theme:      tui.MonoTheme(),
			region:     "legend:up",
			wantTitle:  "Up Count",
			wantBorder: '+',
		},
		{
			name:       "ASCIITheme",
			width:      100,
			height:     30,
			theme:      tui.Theme{Name: "ascii", Frame: tui.BorderASCII, ASCII: true},
			region:     "footer:quit",
			wantTitle:  "Quit Shortcut",
			wantBorder: '+',
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(tc.theme, topProviders(1), clock)
			m.explainerPopup = true
			m.explainerName = tc.region
			m.explainerX = 5
			m.explainerY = 5

			b := tui.New(tc.width, tc.height)
			m.drawExplainerPopup(b)

			lines := []string{}
			foundTitle := false
			foundBorder := false
			for y := 0; y < b.Height(); y++ {
				var sb strings.Builder
				for x := 0; x < b.Width(); x++ {
					c := b.Cell(x, y)
					if c.Rune == tc.wantBorder {
						foundBorder = true
					}
					if c.Rune != 0 {
						sb.WriteRune(c.Rune)
					}
				}
				rowText := sb.String()
				if strings.Contains(rowText, tc.wantTitle) {
					foundTitle = true
				}
				lines = append(lines, rowText)
			}

			if !foundTitle {
				t.Fatalf("%s: title %q not found in rendered popup:\n%s", tc.name, tc.wantTitle, strings.Join(lines, "\n"))
			}
			if !foundBorder {
				t.Fatalf("%s: expected border rune %q not found in rendered popup", tc.name, tc.wantBorder)
			}
		})
	}
}

// TestExplainerMouseHoverAndTouchPin verifies hover display and click-pinning interactions.
func TestExplainerMouseHoverAndTouchPin(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.regions = []topRegion{
		{Name: "now:sessions", Rect: tui.Rect{X: 10, Y: 10, W: 20, H: 1}},
		{Name: "other:empty", Rect: tui.Rect{X: 40, Y: 10, W: 20, H: 1}},
	}

	// 1. MouseMove over a region with an explainer activates popup (not pinned)
	m.handle(tcellui.Event{Kind: tcellui.EventMouse, Action: tcellui.MouseMove, X: 15, Y: 10})
	if !m.explainerPopup || m.explainerName != "now:sessions" || m.explainerPinned {
		t.Fatalf("hover failed: popup=%v name=%q pinned=%v", m.explainerPopup, m.explainerName, m.explainerPinned)
	}

	// 2. MouseMove over an unknown region dismisses popup when unpinned
	m.handle(tcellui.Event{Kind: tcellui.EventMouse, Action: tcellui.MouseMove, X: 45, Y: 10})
	if m.explainerPopup || m.explainerName != "" {
		t.Fatalf("hover leave failed: popup=%v name=%q", m.explainerPopup, m.explainerName)
	}

	// 3. MousePress (touch tap) pins the explainer
	m.handle(tcellui.Event{Kind: tcellui.EventMouse, Action: tcellui.MousePress, Button: tcellui.MouseLeft, X: 15, Y: 10})
	if !m.explainerPopup || m.explainerName != "now:sessions" || !m.explainerPinned {
		t.Fatalf("tap pin failed: popup=%v name=%q pinned=%v", m.explainerPopup, m.explainerName, m.explainerPinned)
	}

	// 4. Moving mouse while pinned preserves the pinned explainer
	m.handle(tcellui.Event{Kind: tcellui.EventMouse, Action: tcellui.MouseMove, X: 0, Y: 0})
	if !m.explainerPopup || m.explainerName != "now:sessions" || !m.explainerPinned {
		t.Fatalf("pinned explainer lost on move: popup=%v name=%q pinned=%v", m.explainerPopup, m.explainerName, m.explainerPinned)
	}

	// 5. Tapping the same region again unpins and dismisses
	m.handle(tcellui.Event{Kind: tcellui.EventMouse, Action: tcellui.MousePress, Button: tcellui.MouseLeft, X: 15, Y: 10})
	if m.explainerPopup || m.explainerPinned {
		t.Fatalf("tap toggle unpin failed: popup=%v pinned=%v", m.explainerPopup, m.explainerPinned)
	}

	// 6. Key press dismisses any open explainer
	m.explainerPopup, m.explainerPinned, m.explainerName = true, true, "now:sessions"
	m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyEsc})
	if m.explainerPopup || m.explainerPinned {
		t.Fatalf("esc dismiss failed: popup=%v pinned=%v", m.explainerPopup, m.explainerPinned)
	}
}

// TestTopGlossaryDocsInSync verifies that docs/urnet-tools-go.md contains every explainer
// term and title from the registry, preventing code and documentation from drifting apart.
func TestTopGlossaryDocsInSync(t *testing.T) {
	docPath := filepath.Join("..", "..", "docs", "urnet-tools-go.md")
	content, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", docPath, err)
	}
	text := string(content)

	catalog := allTopExplainers()
	if len(catalog) == 0 {
		t.Fatal("empty explainer catalog")
	}

	for _, exp := range catalog {
		if !strings.Contains(text, exp.Title) {
			t.Errorf("docs/urnet-tools-go.md missing explainer title: %q (region %s)", exp.Title, exp.Region)
		}
		if !strings.Contains(text, "`"+exp.Region+"`") {
			t.Errorf("docs/urnet-tools-go.md missing explainer region code: `%s`", exp.Region)
		}
	}
}
