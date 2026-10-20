// Command fake-release-server serves a directory of release assets the way the
// GitHub releases API and download CDN do, so the shipped Linux installer can
// be exercised end to end in CI without a real release.
//
// The installer reads two endpoints (both overridable behind
// URNET_ALLOW_BASE_OVERRIDE=1):
//
//	URNET_API_BASE/releases/latest          -> release JSON
//	URNET_API_BASE/releases/tags/<tag>      -> release JSON
//	URNET_MIRROR_BASE/<tag>/<asset>         -> asset bytes
//
// This server answers both shapes plus the github.com download path, and it
// advertises a real sha256 digest for every asset so the installer's digest
// check takes its verified path instead of the legacy fallback.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type asset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
	URL    string `json:"browser_download_url"`
}

type release struct {
	Tag        string  `json:"tag_name"`
	Name       string  `json:"name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	HTMLURL    string  `json:"html_url"`
	URL        string  `json:"url"`
	Assets     []asset `json:"assets"`
}

func main() {
	var (
		dir   = flag.String("dir", ".", "directory holding the release assets")
		addr  = flag.String("addr", "127.0.0.1:18080", "listen address")
		tag   = flag.String("tag", "", "release tag to advertise (required)")
		owner = flag.String("owner", "full-bars", "repository owner in the API path")
		repo  = flag.String("repo", "sn", "repository name in the API path")
	)
	flag.Parse()

	if *tag == "" {
		log.Fatal("fake-release-server: -tag is required")
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatalf("fake-release-server: %v", err)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		log.Fatalf("fake-release-server: %v", err)
	}
	assets := make([]asset, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		sum, err := fileSHA256(filepath.Join(abs, e.Name()))
		if err != nil {
			log.Fatalf("fake-release-server: %v", err)
		}
		assets = append(assets, asset{
			Name:   e.Name(),
			Size:   info.Size(),
			Digest: "sha256:" + sum,
			URL:    fmt.Sprintf("/releases/download/%s/%s", *tag, e.Name()),
		})
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].Name < assets[j].Name })
	if len(assets) == 0 {
		log.Fatalf("fake-release-server: no assets in %s", abs)
	}

	rel := release{
		Tag:     *tag,
		Name:    *tag,
		HTMLURL: fmt.Sprintf("http://%s/%s/%s/releases/tag/%s", *addr, *owner, *repo, *tag),
		URL:     fmt.Sprintf("http://%s/repos/%s/%s/releases/%s", *addr, *owner, *repo, *tag),
		Assets:  assets,
	}
	body, err := json.Marshal(rel)
	if err != nil {
		log.Fatalf("fake-release-server: %v", err)
	}

	apiPrefix := fmt.Sprintf("/repos/%s/%s", *owner, *repo)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc(apiPrefix+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, body)
	})
	mux.HandleFunc(apiPrefix+"/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, body)
	})
	mux.HandleFunc(apiPrefix+"/releases", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, body)
	})
	// Both mirror spellings: the installer's URNET_MIRROR_BASE points at
	// /releases/download, while a bare base is also accepted.
	for _, prefix := range []string{"/releases/download/", "/"} {
		mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
			serveAsset(w, r, abs)
		})
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("fake-release-server: %v", err)
	}
	// The harness polls /healthz; print the line it greps for.
	fmt.Printf("fake-release-server listening on http://%s tag=%s assets=%d\n", ln.Addr(), *tag, len(assets))
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatalf("fake-release-server: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func serveAsset(w http.ResponseWriter, r *http.Request, dir string) {
	name := filepath.Base(strings.TrimSuffix(r.URL.Path, "/"))
	if name == "" || name == "." || name == "/" {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(dir, name)
	// filepath.Base above rules out traversal; confirm the name really is a
	// plain asset in the served directory.
	if filepath.Dir(path) != dir {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
