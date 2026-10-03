package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A proxy_url.json whose CONTENT cannot be parsed (empty after a power loss,
// truncated, hand edited) is not a transient read error. Treating it as one
// skipped every merge cycle forever, because every other reader returns early
// and nothing else rewrites the file, which kept the URL pipeline off. The merge
// path must keep the bad file as evidence and start from an empty cache.
func TestReadURLStateForMergeQuarantinesACorruptFile(t *testing.T) {
	for name, content := range map[string]string{
		"empty":     "",
		"truncated": `{"sources":["https://example.invalid/list"],"cache":{"1.2.3.4:1080":{`,
		"wrongtype": `{"cache":"not-a-map"}`,
		// time.Time.UnmarshalJSON returns a time parse error, which is neither a
		// SyntaxError nor an UnmarshalTypeError
		"badtimestamp": `{"cache":{"1.2.3.4:1080":{"last_probe":"not-a-time"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			withTempHome(t)
			path, err := proxyURLStatePath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}

			state, ok := readURLStateForMerge()
			if !ok || state == nil || state.Cache == nil || len(state.Cache) != 0 {
				t.Fatalf("a corrupt file must yield an empty usable state, got ok=%v state=%+v", ok, state)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("the corrupt file must be moved aside, stat err = %v", err)
			}
			kept, err := os.ReadFile(path + ".corrupt")
			if err != nil || string(kept) != content {
				t.Fatalf("the corrupt content must be kept as evidence, got %q, %v", kept, err)
			}
			// The next cycle reads a missing file as an empty cache and proceeds.
			if _, ok := readURLStateForMerge(); !ok {
				t.Fatal("the cycle after quarantine must proceed")
			}
		})
	}
}

// An I/O error reading the file may be transient and the file may be fine, so it
// must still skip the cycle and leave the file where it is.
func TestReadURLStateForMergeStillSkipsAnIOError(t *testing.T) {
	withTempHome(t)
	path, err := proxyURLStatePath()
	if err != nil {
		t.Fatal(err)
	}
	// A directory where the file should be: ReadFile fails with an I/O error, not
	// a parse error.
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if state, ok := readURLStateForMerge(); ok || state != nil {
		t.Fatalf("an I/O error must skip the cycle, got ok=%v state=%+v", ok, state)
	}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		t.Fatalf("an unreadable path must be left alone, stat = %v, %v", fi, err)
	}
	if _, err := os.Stat(path + ".corrupt"); err == nil {
		t.Fatal("an I/O error must not quarantine anything")
	}
}

func corruptURLStateOnDisk(t *testing.T) {
	t.Helper()
	path, err := proxyURLStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// While proxy_url.json is unreadable the reload cannot know the URL-sourced
// desired set, so it must not cancel URL proxies. A file proxy the operator
// removed from the file it DID read is a different matter: it must still be
// removed, or `proxy remove` is silently never applied for as long as the bad
// file sits there (and on a box with no URL sources nothing ever repairs it).
func TestReload_UnreadableURLCacheStillRemovesAFileProxy(t *testing.T) {
	r, addrs, cancelled := trimFixtureRunning(t, 3)
	for _, a := range addrs {
		e := r.state.Proxies[a]
		e.Source = "file"
		r.state.Proxies[a] = e
	}
	if err := writeProxyState(r.state); err != nil {
		t.Fatal(err)
	}
	// The operator removes the third proxy from the file.
	lines := ""
	for _, s := range r.runningAuth {
		if s.Key() != addrs[2] {
			lines += s.Address + ":u:p\n"
		}
	}
	if err := os.WriteFile(r.sourcePath, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	corruptURLStateOnDisk(t)

	r.reload()
	if got := cancelled.Load(); got != 1 {
		t.Fatalf("the removed file proxy must be cancelled even with an unreadable URL cache, cancelled %d", got)
	}
	if _, still := r.cancelMap[addrs[2]]; still {
		t.Fatal("the removed file proxy is still in the running set")
	}
}

func TestReload_UnreadableURLCacheKeepsURLProxiesRunning(t *testing.T) {
	r, addrs, cancelled := trimFixtureRunning(t, 3)
	r.state.Proxies[addrs[0]] = ProxyEntry{ID: 1, Health: "up", Graded: true, Score: 0.9, Source: "file"}
	r.state.Proxies[addrs[1]] = ProxyEntry{ID: 2, Health: "up", Graded: true, Score: 0.9, Source: "url"}
	r.state.Proxies[addrs[2]] = ProxyEntry{ID: 3, Health: "up", Graded: true, Score: 0.9} // no recorded source
	if err := writeProxyState(r.state); err != nil {
		t.Fatal(err)
	}
	// The file source no longer lists any of the three (it lists one other
	// proxy, so it is not an empty file), so every running proxy looks undesired.
	if err := os.WriteFile(r.sourcePath, []byte("10.0.0.9:1080:u:p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	corruptURLStateOnDisk(t)

	r.reload()
	if got := cancelled.Load(); got != 1 {
		t.Fatalf("only the file proxy may be cancelled while the URL cache is unreadable, cancelled %d", got)
	}
	for _, kept := range []string{addrs[1], addrs[2]} {
		if _, ok := r.cancelMap[kept]; !ok {
			t.Fatalf("%s (URL or unknown source) must keep running while the URL cache is unreadable", kept)
		}
	}
}
