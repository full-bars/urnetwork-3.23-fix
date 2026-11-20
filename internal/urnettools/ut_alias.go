package urnettools

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// utAliasOwned accepts only a symlink resolving to the installed tool.
func utAliasOwned(tool, alias string) bool {
	info, err := os.Lstat(alias)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return false
	}
	target, err := filepath.EvalSymlinks(alias)
	if err != nil {
		return false
	}
	a, err := os.Stat(target)
	if err != nil {
		return false
	}
	b, err := os.Stat(tool)
	return err == nil && os.SameFile(a, b)
}

// ensureUTSymlink leaves existing paths untouched unless they are our link.
func ensureUTSymlink(tool string) error {
	alias := filepath.Join(filepath.Dir(tool), "ut")
	_, err := os.Lstat(alias)
	if err == nil {
		if utAliasOwned(tool, alias) {
			return nil
		}
		return fmt.Errorf("%s already exists and is not the urnet-tools alias", alias)
	}
	if !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(filepath.Base(tool), alias)
}

func removeUTSymlink(tool, alias string) error {
	if !utAliasOwned(tool, alias) {
		return nil
	}
	return os.Remove(alias)
}

func updateUTSymlink(exe string, cfg updateConfig) {
	if runtime.GOOS == "windows" || cfg.NoUTAlias || os.Getenv("URNETWORK_NO_UT_ALIAS") == "1" || !strings.HasPrefix(cfg.ToolAsset, "urnet-tools-") || filepath.Base(exe) != "urnet-tools" {
		return
	}
	if err := ensureUTSymlink(exe); err != nil {
		fmt.Fprintf(os.Stderr, "ut alias: %v\n", err)
	}
}
