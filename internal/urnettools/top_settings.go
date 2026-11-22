package urnettools

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	Theme                 string       // a tui.ThemeNames entry, or "" for the environment's choice
	Graph                 string       // a tui.GraphSymbolNames entry, or "" for braille
	GraphWindow           string       // a topGraphWindowNames entry, or "" for the default level
	ProxyAfterglowSeconds int          // 0 selects the default
	ProxyHeatmapStyle     heatmapStyle // zero selects Glow
	// The operator's arrangement of the top screen, drawn by their own hand:
	// how wide the right column is and where the box dividers sit. Zero or a
	// missing value means the default for the terminal size.
	LayoutSideWidth int            // explicit right column width; 0 = auto
	LayoutDividers  map[string]int // divider name -> content row index
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
		switch {
		case key == "theme":
			if _, ok := tui.ThemeByName(val); ok {
				s.Theme = val
			}
		case key == "graph":
			if _, ok := tui.GraphSymbolsByName(val); ok {
				s.Graph = val
			}
		case key == "proxy.heatmap_style":
			s.ProxyHeatmapStyle = parseHeatmapStyle(val)
		case key == "graph.window":
			if _, ok := topGraphWindowIndex(val); ok {
				s.GraphWindow = val
			}
		case key == "proxy.afterglow_seconds":
			if v, err := strconv.Atoi(val); err == nil && v >= 1 && v <= 60 {
				s.ProxyAfterglowSeconds = v
			}
		case key == "layout.side_width":
			if v, err := strconv.Atoi(val); err == nil && v >= 0 {
				s.LayoutSideWidth = v
			}
		case strings.HasPrefix(key, "layout.divider."):
			if v, err := strconv.Atoi(val); err == nil && v > 0 {
				if s.LayoutDividers == nil {
					s.LayoutDividers = map[string]int{}
				}
				s.LayoutDividers[strings.TrimPrefix(key, "layout.divider.")] = v
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
	if err := mkdirAllForOwner(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".top.conf.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	out := "# urnet-tools top: chosen in the m menu\ntheme = " + s.Theme + "\ngraph = " + s.Graph + "\n"
	out += "graph.window = " + s.GraphWindow + "\n"
	out += "proxy.heatmap_style = " + s.ProxyHeatmapStyle.String() + "\n"
	if s.ProxyAfterglowSeconds > 0 {
		out += fmt.Sprintf("proxy.afterglow_seconds = %d\n", s.ProxyAfterglowSeconds)
	}
	if s.LayoutSideWidth > 0 {
		out += fmt.Sprintf("layout.side_width = %d\n", s.LayoutSideWidth)
	}
	names := make([]string, 0, len(s.LayoutDividers))
	for name := range s.LayoutDividers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out += fmt.Sprintf("layout.divider.%s = %d\n", name, s.LayoutDividers[name])
	}
	_, werr := tmp.WriteString(out)
	if cerr := tmp.Chmod(0o644); werr == nil {
		werr = cerr
	}
	if werr != nil {
		tmp.Close()
		return werr
	}
	// Under sudo with HOME preserved, the config would land root-owned in the
	// invoking user's config dir, so that user's next non-sudo save cannot
	// overwrite it. When we are root, hand the temporary file to the owner of
	// its directory (usually the invoking user) while it is still an open fd,
	// before the rename, so a switcheroo to a symlink cannot redirect the
	// ownership change to some other root-owned file.
	chownConfigFdToDirOwner(tmp, filepath.Dir(path))
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return os.Rename(tmp.Name(), path)
}
