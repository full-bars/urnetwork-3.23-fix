package urnettools

import (
	"fmt"
	"io"
	"strings"
)

// setupCommand is a command and its exact arguments, without shell expansion.
type setupCommand struct {
	name string
	args []string
}

func setupShellWord(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_./:=-+", r))
	}) == -1 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func (c setupCommand) String() string {
	words := []string{setupShellWord(c.name)}
	for _, arg := range c.args {
		words = append(words, setupShellWord(arg))
	}
	return strings.Join(words, " ")
}

// executeExtenderSetupPlan prints and confirms the complete plan before running it.
func executeExtenderSetupPlan(out io.Writer, plan []setupCommand, yes, dryRun bool, confirm func() (bool, error), run func(setupCommand) error) error {
	if len(plan) == 0 {
		return nil
	}
	fmt.Fprintln(out, "Commands to run:")
	for _, c := range plan {
		fmt.Fprintln(out, "  "+c.String())
	}
	if dryRun {
		return nil
	}
	if !yes {
		ok, err := confirm()
		if err != nil {
			return fmt.Errorf("confirmation required; use --yes for non-interactive setup: %w", err)
		}
		if !ok {
			return fmt.Errorf("extender setup cancelled; no commands executed")
		}
	}
	for _, c := range plan {
		if err := run(c); err != nil {
			return fmt.Errorf("%s: %w", c.String(), err)
		}
	}
	return nil
}
