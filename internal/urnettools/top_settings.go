package urnettools

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urnetwork/connect/internal/tui"
)

// What the `top` menu remembers between runs: the color theme and the graph
// style. It lives in one small key=value file under the user's config
// directory. Reading is forgiving (a missing, unreadable or garbled file is
// just "no settings") because a display preference must never stop top from
// starting; writing reports its error so the menu can say the choice did not
// stick.

type topSettings struct {
	Theme string // a tui.ThemeNames entry, or "" for the environment's choice
	Graph string // a tui.GraphSymbolNames entry, or "" for braille
}

// topSettingsPath is ~/.config/urnet-tools/top.conf (or the platform's
// equivalent). Empty when there is no config directory, which turns
// persistence off.
func topSettingsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "urnet-tools", "top.conf")
}

// loadTopSettings reads the file. Unknown keys and comments are skipped, and so
// is anything that does not name a real theme or style, so a stale or hand-
// edited file cannot select something that is not there.
func loadTopSettings(path string) topSettings {
	var s topSettings
	if path == "" {
		return s
	}
	f, err := os.Open(path)
	if err != nil {
		return s
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.Trim(strings.TrimSpace(val), `"`)
		switch key {
		case "theme":
			if _, ok := tui.ThemeByName(val); ok {
				s.Theme = val
			}
		case "graph":
			if _, ok := tui.GraphSymbolsByName(val); ok {
				s.Graph = val
			}
		}
	}
	return s
}

// saveTopSettings writes the file through a temporary file and a rename, so a
// crash mid-write never leaves half a file behind.
func saveTopSettings(path string, s topSettings) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".top.conf.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	_, werr := fmt.Fprintf(tmp, "# urnet-tools top: chosen in the m menu\ntheme = %s\ngraph = %s\n", s.Theme, s.Graph)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	// Under sudo with HOME preserved, the config lands root-owned in the
	// invoking user's config dir, so that user's next non-sudo save cannot
	// overwrite it. When we are root, hand the file to the owner of its
	// directory (usually the invoking user) instead of keeping root ownership.
	chownConfigToDirOwner(path)
	return nil
}
