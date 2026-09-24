package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/connect"
)

// A URL fetch cycle prints several detail lines (per-source probe results, the
// cached-skip count, the tier breakdown). None of them said plainly what
// happened to the pool, and "everything was already known" was the quietest of
// all. The headline is one line, always printed, that says it.
func TestURLCycleSummary(t *testing.T) {
	cases := []struct {
		name string
		in   urlCycleStats
		want string
	}{
		{"new proxies added", urlCycleStats{Sources: 2, Admitted: 12, Held: 3, AlreadyKnown: 48, Rejected: 9, PoolQualified: 340, PoolCached: 371},
			"➕ [proxy][url] cycle: +12 new to the pool from 2 sources (48 already known, 9 rejected, 3 held for re-probe); pool now 340 qualified of 371 cached"},
		{"nothing new is stated plainly", urlCycleStats{Sources: 2, AlreadyKnown: 60, PoolQualified: 340, PoolCached: 371},
			"✔️ [proxy][url] cycle: nothing new from 2 sources (60 already known, 0 rejected); pool 340 qualified of 371 cached"},
		{"one source, singular", urlCycleStats{Sources: 1, Admitted: 1, AlreadyKnown: 0, PoolQualified: 1, PoolCached: 1},
			"➕ [proxy][url] cycle: +1 new to the pool from 1 source (0 already known, 0 rejected); pool now 1 qualified of 1 cached"},
		{"a failed source is called out", urlCycleStats{Sources: 3, Failed: 1, Admitted: 5, AlreadyKnown: 10, PoolQualified: 50, PoolCached: 60},
			"➕ [proxy][url] cycle: +5 new to the pool from 2 of 3 sources, 1 failed (10 already known, 0 rejected); pool now 50 qualified of 60 cached"},
		{"every source failed", urlCycleStats{Sources: 2, Failed: 2, PoolQualified: 50, PoolCached: 60},
			"⚠️ [proxy][url] cycle: every source failed (2 of 2); pool unchanged at 50 qualified of 60 cached"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.String(); got != tc.want {
				t.Fatalf("\n got: %s\nwant: %s", got, tc.want)
			}
			if !isImportantLogLine(tc.in.String()) {
				t.Fatalf("the cycle headline must survive the important-log filter: %q", tc.in.String())
			}
		})
	}
}

// When URL proxies are started, say so once, with how many are held back. The
// launch is what an operator actually cares about, and it used to be a bare
// "+N added" with no source.
func TestURLLaunchLine(t *testing.T) {
	if got := urlLaunchLine(0, 0); got != "" {
		t.Fatalf("no URL proxies added should print nothing, got %q", got)
	}
	got := urlLaunchLine(12, 0)
	if got != "🚀 [proxy][url] launching 12 new URL-sourced proxies" {
		t.Fatalf("got %q", got)
	}
	got = urlLaunchLine(12, 5)
	if got != "🚀 [proxy][url] launching 7 of 12 new URL-sourced proxies (5 held until the file proxies finish warming up)" {
		t.Fatalf("got %q", got)
	}
	if !isImportantLogLine(got) {
		t.Fatalf("the launch line must survive the important-log filter: %q", got)
	}
}

func TestReloadSourceBreakdown(t *testing.T) {
	mk := func(addr string) *connect.ProxySettings { return &connect.ProxySettings{Address: addr} }
	added := []*connect.ProxySettings{mk("a:1"), mk("b:1"), mk("c:1"), mk("d:1"), mk("e:1")}
	sourceOf := map[string]string{"a:1": "url", "b:1": "url", "c:1": "url", "d:1": "file", "e:1": ""}

	if got := reloadSourceBreakdown(added, sourceOf); got != " (url 3, file 1, other 1)" {
		t.Fatalf("got %q", got)
	}
	if got := reloadSourceBreakdown(nil, sourceOf); got != "" {
		t.Fatalf("nothing added should add nothing to the line, got %q", got)
	}
	onlyFile := []*connect.ProxySettings{mk("d:1")}
	if got := reloadSourceBreakdown(onlyFile, sourceOf); got != " (file 1)" {
		t.Fatalf("got %q", got)
	}
	// The existing summary prefix is unchanged: tooling that matches
	// "reloaded: +N added" keeps working.
	line := "🔄 [proxy] reloaded: +5 added" + reloadSourceBreakdown(added, sourceOf) + ", -0 removed"
	if !strings.Contains(line, "reloaded: +5 added") {
		t.Fatalf("summary prefix changed: %q", line)
	}
}

func cycleLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "[proxy][url] cycle:") {
			lines = append(lines, l)
		}
	}
	return lines
}

// End to end through a real fetch cycle: every cycle prints exactly one headline
// that says what happened to the pool, including the quiet "nothing new" cycle.
func TestFetchCyclePrintsOneHeadlineAndSaysWhenNothingIsNew(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	// One proxy that qualifies (a faithful transparent proxy the probe trusts)
	// and one bare SOCKS5 listener that does not.
	ca := newTestCA(t)
	leaf := issueLeafForHost(t, ca, "127.0.0.1")
	withProbeTLSRoot(t, ca)
	goodAddr, goodCleanup := listenSocks5ApiOKTLS(t, &leaf)
	defer goodCleanup()
	badAddr, badCleanup := listenSocks5Once(t)
	defer badCleanup()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(goodAddr + ":user:pass\n" + badAddr + "\n"))
	}))
	defer srv.Close()

	first := captureTlog(t, func() {
		fetchAndMergeProxyURLs(context.Background(), []string{srv.URL}, 0, "127.0.0.1", 1)
	})
	lines := cycleLines(first)
	if len(lines) != 1 {
		t.Fatalf("first cycle printed %d headline lines, want exactly 1:\n%s", len(lines), first)
	}
	for _, want := range []string{"+1 new to the pool from 1 source", "0 already known", "1 rejected", "1 held for re-probe", "1 qualified of 2 cached"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("first cycle headline %q is missing %q", lines[0], want)
		}
	}

	// The same source again: every address is now cached, and the headline says
	// so in plain words instead of leaving it to a "skipped N" line among the
	// detail.
	second := captureTlog(t, func() {
		fetchAndMergeProxyURLs(context.Background(), []string{srv.URL}, 0, "127.0.0.1", 1)
	})
	lines = cycleLines(second)
	if len(lines) != 1 {
		t.Fatalf("second cycle printed %d headline lines, want exactly 1:\n%s", len(lines), second)
	}
	for _, want := range []string{"nothing new from 1 source", "2 already known", "0 rejected", "1 qualified of 2 cached"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("second cycle headline %q is missing %q", lines[0], want)
		}
	}
}

func TestFetchCycleHeadlineWhenTheSourceFails(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	out := captureTlog(t, func() {
		fetchAndMergeProxyURLs(context.Background(), []string{srv.URL}, 0, "", 0)
	})
	lines := cycleLines(out)
	if len(lines) != 1 || !strings.Contains(lines[0], "every source failed (1 of 1)") {
		t.Fatalf("a failing source must say so in the headline, got %v\n%s", lines, out)
	}
}

func urlReloadOutput(t *testing.T, warmupDone bool, urlAddrs ...string) string {
	t.Helper()
	withTempHome(t)
	cache := map[string]ProxyURLEntry{}
	for _, a := range urlAddrs {
		cache[a] = ProxyURLEntry{}
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: cache}); err != nil {
		t.Fatal(err)
	}
	state := &ProxyState{Proxies: map[string]ProxyEntry{}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}
	proxyWarmupDone.Store(warmupDone)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	reloader := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{},
		cancelMapMu: &sync.Mutex{},
		state:       state,
		parentCtx:   context.Background(),
		wg:          &sync.WaitGroup{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			<-proxyCtx.Done()
		},
		drainingProxies: map[string]context.CancelFunc{},
	}
	return captureTlog(t, func() { reloader.reload() })
}

// When URL proxies are added the reload says so: the summary attributes them to
// their source, and one launch line says how many are starting.
func TestReloadAttributesAddedProxiesToTheirSourceAndAnnouncesTheLaunch(t *testing.T) {
	out := urlReloadOutput(t, true, "5.5.5.5:1080", "6.6.6.6:1080", "7.7.7.7:1080")
	if !strings.Contains(out, "reloaded: +3 added (url 3)") {
		t.Fatalf("the summary should attribute the additions to the URL source:\n%s", out)
	}
	if !strings.Contains(out, "[proxy][url] launching 3 new URL-sourced proxies") || strings.Contains(out, "held until") {
		t.Fatalf("the launch line is missing or claims some are held:\n%s", out)
	}
}

// Before file proxies finish warming up, unproven URL proxies wait. The launch
// line says how many, instead of the operator wondering why +3 added shows no
// proxies coming up.
func TestReloadLaunchLineSaysHowManyURLProxiesAreHeldForWarmup(t *testing.T) {
	out := urlReloadOutput(t, false, "5.5.5.5:1080", "6.6.6.6:1080", "7.7.7.7:1080")
	want := "[proxy][url] launching 0 of 3 new URL-sourced proxies (3 held until the file proxies finish warming up)"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in:\n%s", want, out)
	}
}

func TestReloadWithNoURLProxiesPrintsNoLaunchLine(t *testing.T) {
	// A reload that adds only file proxies must not print a URL launch line.
	withTempHome(t)
	state := &ProxyState{Proxies: map[string]ProxyEntry{}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}
	tmpFile := t.TempDir() + "/proxy.txt"
	if err := os.WriteFile(tmpFile, []byte("5.5.5.5:1080:alice:secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reloader := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{},
		cancelMapMu: &sync.Mutex{},
		state:       state,
		sourcePath:  tmpFile,
		parentCtx:   context.Background(),
		wg:          &sync.WaitGroup{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			<-proxyCtx.Done()
		},
		drainingProxies: map[string]context.CancelFunc{},
	}
	out := captureTlog(t, func() { reloader.reload() })
	if strings.Contains(out, "[proxy][url] launching") {
		t.Fatalf("a file-only reload printed a URL launch line:\n%s", out)
	}
	if !strings.Contains(out, "reloaded: +1 added (file 1)") {
		t.Fatalf("the file addition should be attributed to the file source:\n%s", out)
	}
}

// Each source gets its own line saying how many of ITS proxies were added, so
// the operator can tell which source produces and which is dead weight. The
// label is host and path only: source URLs often carry tokens in the query.
func TestURLSourceLabelDropsSecretsAndDisambiguates(t *testing.T) {
	urls := []string{
		"https://lists.example.com/proxies/http.txt?token=SECRET&x=1",
		"https://user:hunter2@other.example.org/raw#frag",
		"https://lists.example.com/proxies/http.txt?token=OTHER",
		"not a url at all?key=SECRET",
	}
	got := urlSourceLabels(urls)
	want := []string{
		"lists.example.com/proxies/http.txt",
		"other.example.org/raw",
		"lists.example.com/proxies/http.txt #2",
		"not a url at all",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("label[%d] = %q, want %q", i, got[i], want[i])
		}
		if strings.Contains(got[i], "SECRET") || strings.Contains(got[i], "hunter2") || strings.Contains(got[i], "OTHER") {
			t.Errorf("label[%d] leaks a secret: %q", i, got[i])
		}
	}
	long := urlSourceLabels([]string{"https://example.com/" + strings.Repeat("a", 200)})[0]
	if len(long) > 64 {
		t.Errorf("a very long label is not capped: %d chars", len(long))
	}
}

func TestURLSourceStatsLine(t *testing.T) {
	cases := []struct {
		name string
		in   urlSourceStats
		want string
	}{
		{"added", urlSourceStats{Label: "a.example/list.txt", Lines: 42, Known: 30, Dead: 2, Rejected: 2, Added: 8},
			"📥 [proxy][url] source a.example/list.txt: +8 new of 42 listed (30 already known, 2 rejected, 2 dead)"},
		{"nothing new", urlSourceStats{Label: "b.example/x", Lines: 60, Known: 60},
			"📥 [proxy][url] source b.example/x: nothing new of 60 listed (60 already known, 0 rejected, 0 dead)"},
		{"failed", urlSourceStats{Label: "c.example/x", Failed: true},
			"📥 [proxy][url] source c.example/x: fetch failed"},
		{"empty list", urlSourceStats{Label: "d.example/x"},
			"📥 [proxy][url] source d.example/x: nothing new of 0 listed (0 already known, 0 rejected, 0 dead)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.String(); got != tc.want {
				t.Fatalf("\n got: %s\nwant: %s", got, tc.want)
			}
			if !isImportantLogLine(tc.in.String()) {
				t.Fatalf("the per-source line must survive the important-log filter")
			}
		})
	}
}

// Two sources, one of them repeating the first one's proxy: the per-source lines
// attribute each new proxy to the first source that listed it, and the headline
// is the sum.
func TestFetchCyclePrintsOneLinePerSourceWithItsOwnCounts(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	ca := newTestCA(t)
	leaf := issueLeafForHost(t, ca, "127.0.0.1")
	withProbeTLSRoot(t, ca)
	goodA, cleanA := listenSocks5ApiOKTLS(t, &leaf)
	defer cleanA()
	goodB, cleanB := listenSocks5ApiOKTLS(t, &leaf)
	defer cleanB()
	bad, cleanBad := listenSocks5Once(t)
	defer cleanBad()

	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(goodA + ":user:pass\n" + bad + "\n"))
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(goodA + ":user:pass\n" + goodB + ":user:pass\n"))
	}))
	defer srvB.Close()
	srvDead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srvDead.Close()

	out := captureTlog(t, func() {
		fetchAndMergeProxyURLs(context.Background(), []string{srvA.URL + "/a.txt?token=SECRET", srvB.URL + "/b.txt", srvDead.URL + "/c.txt"}, 0, "127.0.0.1", 1)
	})
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "[proxy][url] source ") || strings.Contains(l, "[proxy][url] cycle:") {
			t.Log(l) // the operator's view of one cycle
		}
	}
	var sources []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "[proxy][url] source ") {
			sources = append(sources, l)
		}
	}
	if len(sources) != 3 {
		t.Fatalf("want one line per source (3), got %d:\n%s", len(sources), out)
	}
	for i, want := range []string{
		"/a.txt: +1 new of 2 listed (0 already known, 1 rejected, 0 dead)", // goodA qualifies; the bare socks5 does not
		"/b.txt: +1 new of 2 listed (1 already known, 0 rejected, 0 dead)", // goodA was A's; goodB is new
		"/c.txt: fetch failed",
	} {
		if !strings.Contains(sources[i], want) {
			t.Errorf("source line %d = %q, want it to contain %q", i, sources[i], want)
		}
		if strings.Contains(sources[i], "SECRET") {
			t.Errorf("source line %d leaks the URL's token: %q", i, sources[i])
		}
	}
	lines := cycleLines(out)
	if len(lines) != 1 || !strings.Contains(lines[0], "+2 new to the pool from 2 of 3 sources, 1 failed") {
		t.Fatalf("headline should be the sum of the sources: %v", lines)
	}
}
