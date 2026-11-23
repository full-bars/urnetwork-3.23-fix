package urnettools

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/urnetwork/connect/internal/brand"
)

// The URnetwork logo in urnet-tools output. The artwork and its renderer live
// in internal/brand; this file decides whether the artwork may be drawn at all
// and lays it out for the version line, the help pages and status.
//
// The logo is decoration, so every rule here fails towards printing nothing but
// the bytes the tool printed before the artwork existed. That keeps pipes,
// scripts and every machine-readable mode byte for byte what they were.

var (
	// logoStdoutIsTerminal reports whether stdout is a terminal. A pipe or a
	// redirected file never sees the artwork.
	logoStdoutIsTerminal = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }
	// logoStdoutSize returns the terminal size behind stdout.
	logoStdoutSize = func() (int, int, bool) {
		w, h, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil || w <= 0 || h <= 0 {
			return 0, 0, false
		}
		return w, h, true
	}
	// logoStderrIsTerminal and logoStderrSize are the same checks for stderr,
	// where the hand-written help pages are written.
	logoStderrIsTerminal = func() bool { return term.IsTerminal(int(os.Stderr.Fd())) }
	logoStderrSize       = func() (int, int, bool) {
		w, h, err := term.GetSize(int(os.Stderr.Fd()))
		if err != nil || w <= 0 || h <= 0 {
			return 0, 0, false
		}
		return w, h, true
	}
	logoGetenv = os.Getenv
	// logoPrefsPath is the file that holds the ui.logo setting. It is the same
	// key=value file that remembers the top preferences.
	logoPrefsPath = topSettingsPath
	// logoOffByFlag records --no-logo for this run.
	logoOffByFlag = false
)

// logoMode names the output modes in which the logo is never drawn, because
// their bytes are read by something other than a person.
type logoMode struct {
	Plain bool // --plain
	JSON  bool // --json and --json-summary
	Brief bool // --brief
}

// logoAllowed reports whether the logo may be drawn for this run. The mode, the
// flag, the environment, the saved setting and stdout all have to agree.
func logoAllowed(m logoMode) bool { return logoAllowedOn(m, logoStdoutIsTerminal) }

// logoAllowedOn is logoAllowed for the stream the output goes to: the artwork
// follows the terminal that receives it.
func logoAllowedOn(m logoMode, isTerminal func() bool) bool {
	if logoOffByFlag || m.Plain || m.JSON || m.Brief {
		return false
	}
	if on, ok := parseOnOff(logoGetenv("URNET_NO_LOGO")); ok && on {
		return false
	}
	if !loadTopSettings(logoPrefsPath()).logoOn() {
		return false
	}
	return isTerminal()
}

// stripLogoFlag removes --no-logo from the arguments and records it. The flag is
// global, so it is taken out here, once, instead of in every command's own
// parser. It is put back in front of the arguments of a cross-user re-exec so
// the elevated run hides the logo too.
func stripLogoFlag(args []string) []string {
	kept := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--no-logo" {
			logoOffByFlag = true
			continue
		}
		kept = append(kept, a)
	}
	return kept
}

// logoCapabilities describes what the terminal behind stdout can draw.
func logoCapabilities() brand.Capabilities {
	env := logoGetenv
	caps := brand.Capabilities{Depth: brand.Color16, UTF8: true}
	if env("NO_COLOR") != "" {
		caps.NoColor = true
		caps.Depth = brand.Mono
	}
	switch t := env("TERM"); {
	case t == "" || t == "dumb":
		// No block glyphs: the renderer falls back to the name on one line.
		caps.UTF8 = false
	case strings.Contains(t, "256"):
		caps.Depth = brand.Color256
	}
	switch strings.ToLower(env("COLORTERM")) {
	case "truecolor", "24bit":
		caps.Depth = brand.TrueColor
	}
	return caps
}

// logoArt returns the named artwork rendered for this terminal and the cell
// width it occupies, or "" when the terminal is too narrow or too short for it.
func logoArt(name string, minWidth int) (string, int) {
	return logoArtOn(name, minWidth, logoStdoutSize)
}

// logoArtOn is logoArt for the terminal that size describes.
func logoArtOn(name string, minWidth int, size func() (int, int, bool)) (string, int) {
	w, h, ok := size()
	if !ok || w < minWidth {
		return "", 0
	}
	a := brand.Assets[name]
	if a == nil || h < a.Rows {
		return "", 0
	}
	caps := logoCapabilities()
	if !caps.UTF8 {
		// A terminal that cannot show block characters gets the plain output
		// it had before, not a one line name in the middle of a paragraph.
		return "", 0
	}
	return brand.Render(a, caps), a.Cols
}

// logoIcon8 is the small icon used beside the version line and a subcommand's
// first help paragraph.
func logoIcon8() (string, int) { return logoArt("icon-flat-8x4", 40) }

// logoIcon12 is the icon used beside the root help title.
func logoIcon12() (string, int) { return logoArt("icon-flat-12x6", 60) }

// logoWordmark picks the wordmark that fits the terminal width: the 100 column
// one at 120 columns, the 80 column one at 100, nothing below that.
func logoWordmark() (string, int) {
	if art, cols := logoArt("wordmark-100x7", 120); art != "" {
		return art, cols
	}
	return logoArt("wordmark-80x6", 100)
}

// printToolVersion prints the tool version, with the small icon beside it when
// the output is a terminal. Without a terminal it prints the version alone, so
// a script that reads it sees the same bytes as before.
func printToolVersion() {
	if logoAllowed(logoMode{}) {
		if art, cols := logoIcon8(); art != "" {
			fmt.Fprintln(os.Stdout, brand.Beside(art, cols, ToolVersion, true))
			return
		}
	}
	fmt.Fprintln(os.Stdout, ToolVersion)
}

// printStatusLogo prints the wordmark above the status blocks. It leaves one
// blank line after the artwork so the blocks keep their own spacing.
func printStatusLogo(m logoMode) {
	if !logoAllowed(m) {
		return
	}
	if art, _ := logoWordmark(); art != "" {
		fmt.Fprintln(os.Stdout, art)
		fmt.Fprintln(os.Stdout)
	}
}

// logoParagraph puts the small icon beside a command's first help paragraph.
// It is the one hook the per-command help template needs.
func logoParagraph(text string) string {
	return logoParagraphOn(text, logoStdoutIsTerminal, logoStdoutSize)
}

// logoParagraphOn is logoParagraph for the stream the text is written to.
func logoParagraphOn(text string, isTerminal func() bool, size func() (int, int, bool)) string {
	if !logoAllowedOn(logoMode{}, isTerminal) {
		return text
	}
	art, cols := logoArtOn("icon-flat-8x4", 40, size)
	if art == "" {
		return text
	}
	return brand.Beside(art, cols, text, true)
}

// logoTitle puts the twelve column icon beside the root help title line.
func logoTitle(title string) string {
	if !logoAllowed(logoMode{}) {
		return title
	}
	art, cols := logoIcon12()
	if art == "" {
		return title
	}
	return brand.Beside(art, cols, title, true)
}

// The help templates call these, so they are registered once at start.
func init() {
	cobra.AddTemplateFunc("logoParagraph", logoParagraph)
	cobra.AddTemplateFunc("logoTitle", logoTitle)
}

// logoHelp is logoParagraph for a hand-written help page: the icon goes
// beside the page's first paragraph and the rest follows unchanged. These pages
// are written to stderr, so the terminal check and the width come from stderr.
func logoHelp(page string) string {
	head, rest, ok := strings.Cut(page, "\n\n")
	if !ok {
		return logoParagraphOn(page, logoStderrIsTerminal, logoStderrSize)
	}
	return logoParagraphOn(head, logoStderrIsTerminal, logoStderrSize) + "\n\n" + rest
}
