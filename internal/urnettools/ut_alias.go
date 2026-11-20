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

// captureUTCopy records ownership before either Windows executable is replaced.
func captureUTCopy(exe string, cfg updateConfig) bool {
	if runtime.GOOS != "windows" || cfg.NoUTAlias || os.Getenv("URNETWORK_NO_UT_ALIAS") == "1" || !strings.HasPrefix(cfg.ToolAsset, "urnet-tools-") {
		return false
	}
	dir := filepath.Dir(exe)
	tool, alias := filepath.Join(dir, "urnet-tools.exe"), filepath.Join(dir, "ut.exe")
	info, err := os.Lstat(alias)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	a, e1 := fileSHA256(tool)
	b, e2 := fileSHA256(alias)
	return e1 == nil && e2 == nil && a == b
}

func updateUTAlias(exe string, cfg updateConfig, owned bool) {
	if runtime.GOOS == "windows" {
		if cfg.NoUTAlias || os.Getenv("URNETWORK_NO_UT_ALIAS") == "1" || !strings.HasPrefix(cfg.ToolAsset, "urnet-tools-") {
			return
		}
		dir := filepath.Dir(exe)
		tool, alias := filepath.Join(dir, "urnet-tools.exe"), filepath.Join(dir, "ut.exe")
		if !owned {
			fmt.Fprintf(os.Stderr, "ut alias: %s already exists and is not the urnet-tools copy\n", alias)
			return
		}
		other := alias
		if strings.EqualFold(filepath.Base(exe), "ut.exe") {
			other = tool
		}
		if err := installBinary(exe, other, ""); err != nil {
			fmt.Fprintf(os.Stderr, "ut alias: %v\n", err)
		}
		return
	}
	updateUTSymlink(exe, cfg)
}

func updateUTSymlink(exe string, cfg updateConfig) {
	if runtime.GOOS == "windows" || cfg.NoUTAlias || os.Getenv("URNETWORK_NO_UT_ALIAS") == "1" || !strings.HasPrefix(cfg.ToolAsset, "urnet-tools-") || filepath.Base(exe) != "urnet-tools" {
		return
	}
	if err := ensureUTSymlink(exe); err != nil {
		fmt.Fprintf(os.Stderr, "ut alias: %v\n", err)
	}
}

// removeInstalledUTAlias removes only an alias owned by the adjacent tool.
func removeInstalledUTAlias(dir string) error {
	if runtime.GOOS != "windows" {
		tool := filepath.Join(dir, "urnet-tools")
		paths := []string{filepath.Join(dir, "ut"), "/usr/local/bin/ut"}
		if home, err := os.UserHomeDir(); err == nil {
			paths = append(paths, filepath.Join(home, ".local", "bin", "ut"))
		}
		for _, alias := range paths {
			if err := removeUTSymlink(tool, alias); err != nil {
				return err
			}
		}
		return nil
	}
	tool, alias := filepath.Join(dir, "urnet-tools.exe"), filepath.Join(dir, "ut.exe")
	info, err := os.Lstat(alias)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	a, e1 := fileSHA256(tool)
	b, e2 := fileSHA256(alias)
	if e1 != nil || e2 != nil || a != b {
		return nil
	}
	return os.Remove(alias)
}
