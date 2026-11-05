package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// refreshParityFixture is a running provider with four kinds of tracked proxy:
// two still in the curated file, one the operator dropped from the file, one
// URL-cached proxy that is still cached, and one URL proxy the cache no longer
// holds.
type refreshParityFixture struct {
	r         *ProxyReloader
	file      string
	running   []string
	cancelled map[string]bool
	mu        sync.Mutex
}

func fileSettings(addr string) *connect.ProxySettings {
	return &connect.ProxySettings{Network: "tcp", Address: addr, Auth: &proxy.Auth{User: "u", Password: "p"}}
}

func urlSettings(addr string) *connect.ProxySettings {
	return &connect.ProxySettings{Network: "tcp", Address: addr}
}

func newRefreshParityFixture(t *testing.T) *refreshParityFixture {
	t.Helper()
	withTempHome(t)
	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	f := &refreshParityFixture{cancelled: map[string]bool{}}
	f.file = t.TempDir() + "/proxy.txt"

	fileKeep1, fileKeep2, fileStale := fileSettings("10.0.0.1:1080"), fileSettings("10.0.0.2:1080"), fileSettings("10.0.0.3:1080")
	urlKeep, urlGone := urlSettings("20.0.0.1:1080"), urlSettings("20.0.0.2:1080")

	if err := os.WriteFile(f.file, []byte("10.0.0.1:1080:u:p\n10.0.0.2:1080:u:p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{urlKeep.Address: {}}}); err != nil {
		t.Fatal(err)
	}

	parent, cancelParent := context.WithCancel(context.Background())
	state := &ProxyState{StartedAt: time.Now().Add(-24 * time.Hour), Source: f.file, Proxies: map[string]ProxyEntry{
		fileKeep1.Key(): {ID: 1, Health: "up", Source: "file"},
		fileKeep2.Key(): {ID: 2, Health: "up", Source: "file"},
		fileStale.Key(): {ID: 3, Health: "up", Source: "file"},
		urlKeep.Key():   {ID: 4, Health: "up", Source: "url"},
		urlGone.Key():   {ID: 5, Health: "up", Source: "url"},
	}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}
	f.r = &ProxyReloader{
		cancelMap:       map[string]context.CancelFunc{},
		cancelMapMu:     &sync.Mutex{},
		state:           state,
		sourcePath:      f.file,
		parentCtx:       parent,
		wg:              &sync.WaitGroup{},
		spawnProxy:      func(context.Context, *connect.ProxySettings, bool, bool) { <-parent.Done() },
		drainingProxies: map[string]context.CancelFunc{},
	}
	all := []*connect.ProxySettings{fileKeep1, fileKeep2, fileStale, urlKeep, urlGone}
	f.r.seedRunningAuth(all)
	for _, s := range all {
		key := s.Key()
		f.running = append(f.running, key)
		f.r.cancelMap[key] = func() { f.mu.Lock(); f.cancelled[key] = true; f.mu.Unlock() }
	}
	t.Cleanup(func() { cancelParent(); f.r.wg.Wait() })
	return f
}

// previewRemoved is what `proxy refresh` lists as removed, read from the
// persisted proxy.state exactly as the command reads it.
func (f *refreshParityFixture) previewRemoved(t *testing.T) []string {
	t.Helper()
	state, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	_, removed, err := planProxyRefreshFromState(state)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, rp := range removed {
		out = append(out, rp.key)
	}
	sort.Strings(out)
	return out
}

// reloadRemoved runs the real reload() and returns the proxies it cancelled.
func (f *refreshParityFixture) reloadRemoved() []string {
	f.r.reload()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for _, key := range f.running {
		if f.cancelled[key] {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

func (f *refreshParityFixture) assertParity(t *testing.T, want []string) {
	t.Helper()
	sort.Strings(want)
	preview := f.previewRemoved(t)
	reloaded := f.reloadRemoved()
	if !reflect.DeepEqual(preview, reloaded) {
		t.Fatalf("preview and reload disagree: preview removes %q, reload cancelled %q", preview, reloaded)
	}
	if !reflect.DeepEqual(reloaded, want) {
		t.Fatalf("removed %q, want %q", reloaded, want)
	}
}

// The core defect: the preview diffed proxy.state against the curated file
// alone, so every URL-cached proxy showed as "Removing" while reload() kept it.
func TestRefreshPreview_MatchesReload_URLCachedProxiesAreKept(t *testing.T) {
	f := newRefreshParityFixture(t)
	f.assertParity(t, []string{
		fileSettings("10.0.0.3:1080").Key(), // dropped from the curated file
		urlSettings("20.0.0.2:1080").Key(),  // no longer in the URL cache
	})
}

// A curated file that reads as empty while the URL cache is populated: reload
// treats the URL cache as the whole desired set, so the file proxies go and
// the cached URL proxy stays. The preview must say the same.
func TestRefreshPreview_MatchesReload_EmptyCuratedFileWithURLCache(t *testing.T) {
	f := newRefreshParityFixture(t)
	if err := os.WriteFile(f.file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.assertParity(t, []string{
		fileSettings("10.0.0.1:1080").Key(),
		fileSettings("10.0.0.2:1080").Key(),
		fileSettings("10.0.0.3:1080").Key(),
		urlSettings("20.0.0.2:1080").Key(),
	})
}

// An unreadable proxy_url.json makes the URL-sourced desired state unknown:
// reload leaves URL proxies alone and still removes the dropped file proxy.
func TestRefreshPreview_MatchesReload_UnreadableURLCache(t *testing.T) {
	f := newRefreshParityFixture(t)
	corruptURLStateOnDisk(t)
	f.assertParity(t, []string{fileSettings("10.0.0.3:1080").Key()})
}

// An unreadable curated file is an error for the preview, as reload() skips.
func TestRefreshPreview_UnreadableCuratedFileIsAnError(t *testing.T) {
	f := newRefreshParityFixture(t)
	if err := os.Remove(f.file); err != nil {
		t.Fatal(err)
	}
	state, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := planProxyRefreshFromState(state); err == nil {
		t.Fatal("an unreadable curated file must fail the preview, not diff against zero proxies")
	}
	if removed := f.reloadRemoved(); len(removed) != 0 {
		t.Fatalf("reload must skip on an unreadable curated file, cancelled %q", removed)
	}
}

// The curated-only path keeps diffing against that file alone: no URL cache,
// no URL sources, so the shared builder adds nothing of its own.
func TestRefreshPreview_CuratedOnlyDiffsAgainstTheFileAlone(t *testing.T) {
	withTempHome(t)
	file := t.TempDir() + "/proxy.txt"
	if err := os.WriteFile(file, []byte("10.0.0.1:1080:u:p\n10.0.0.9:1080:u:p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	kept, gone := fileSettings("10.0.0.1:1080"), fileSettings("10.0.0.2:1080")
	state := &ProxyState{Source: file, Proxies: map[string]ProxyEntry{
		kept.Key(): {ID: 1, Health: "up"},
		gone.Key(): {ID: 2, Health: "dead"},
	}}
	added, removed, err := planProxyRefreshFromState(state)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(added, []string{fileSettings("10.0.0.9:1080").Key()}) {
		t.Fatalf("added=%q", added)
	}
	if len(removed) != 1 || removed[0].key != gone.Key() {
		t.Fatalf("removed=%+v, want only the proxy dropped from the file", removed)
	}
}

// Structural guard: proxyRefresh must take its desired set from the shared
// builder. Behavioural tests cannot drive proxyRefresh itself (it prompts and
// exits the process), so this fails if the command goes back to reading the
// curated source on its own, which is what let the preview drift.
func TestProxyRefreshUsesTheReloadersDesiredSet(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.BlockStmt
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "proxyRefresh" && fn.Recv == nil {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("proxyRefresh not found in main.go")
	}
	called := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok {
				called[id.Name] = true
			}
		}
		return true
	})
	if !called["planProxyRefreshFromState"] {
		t.Fatal("proxyRefresh must diff through planProxyRefreshFromState (the reloader's desired-set builder)")
	}
	for _, own := range []string{"readProxySettings", "readProxySettingsFromFile", "planProxyRefresh"} {
		if called[own] {
			t.Fatalf("proxyRefresh calls %s itself; its desired set must come from the reloader's builder", own)
		}
	}
}
