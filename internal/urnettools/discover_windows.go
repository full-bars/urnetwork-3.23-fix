//go:build windows

package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// discoverProcesses on Windows enumerates running provider processes with the
// toolhelp snapshot API (not tasklist CSV, which is locale-fragile) and fills
// Provider records. There are no systemd units on Windows; the provider runs
// as a user-level process, so Unit stays empty and Binary points at the
// running executable.
func discoverProcesses() []Provider {
	var out []Provider

	// Running processes first.
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err == nil {
		defer windows.CloseHandle(snapshot)
		var entry windows.ProcessEntry32
		entry.Size = uint32(unsafe.Sizeof(entry))
		err = windows.Process32First(snapshot, &entry)
		for err == nil {
			name := windows.UTF16ToString(entry.ExeFile[:])
			if strings.EqualFold(name, "urnetwork.exe") {
				exe := resolveProcessExe(int(entry.ProcessID), name)
				out = append(out, Provider{
					User:     currentUser(),
					StateDir: windowsStateDir(),
					Binary:   exe,
					PID:      int(entry.ProcessID),
					Running:  true,
				})
			}
			err = windows.Process32Next(snapshot, &entry)
		}
	}

	// If nothing is running, fall back to the standard install location so
	// auto-start/auto-update still have a target on a stopped install.
	if len(out) == 0 {
		if exe := installedProviderBinary(); exe != "" {
			out = append(out, Provider{
				User:     currentUser(),
				StateDir: windowsStateDir(),
				Binary:   exe,
				Running:  false,
			})
		}
	}
	return out
}

// resolveProcessExe returns the full path of a running process by its pid,
// falling back to the image name when the path cannot be resolved.
func resolveProcessExe(pid int, imageName string) string {
	// QueryProcessImageFileName via the toolhelp pid is not directly
	// available; open the process and read the image path.
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err == nil {
		defer windows.CloseHandle(h)
		var buf [windows.MAX_PATH]uint16
		var size uint32 = windows.MAX_PATH
		if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err == nil && size > 0 {
			return windows.UTF16ToString(buf[:size])
		}
	}
	return imageName
}

// installedProviderBinary locates the provider in the standard Windows
// install directory (%LOCALAPPDATA%\urnetwork\provider\urnetwork.exe).
func installedProviderBinary() string {
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		return ""
	}
	candidates := []string{
		filepath.Join(localAppData, "urnetwork", "provider", "urnetwork.exe"),
		filepath.Join(localAppData, "urnetwork", "urnetwork.exe"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

// windowsStateDir is the provider state dir on Windows. The provider
// resolves state via os.UserHomeDir() -> %USERPROFILE%\.urnetwork\jwt
// (main.go), NOT %LOCALAPPDATA%. Returning the wrong dir made status
// report a false state dir and uninstall leave the JWT on disk
// (heavyweight review S5).
func windowsStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".urnetwork")
}

// currentUser returns the Windows username for Provider.User. %USERNAME%
// is always set in a Windows process environment; no API call needed.
func currentUser() string {
	return os.Getenv("USERNAME")
}

// discoverStopped on Windows is a no-op: there are no systemd units; the
// provider runs as a user-level process and the process scan is the only
// discovery source.
func discoverStopped(running []Provider) []Provider {
	return nil
}

// currentUserName is the shared selection-code entry point on Windows. It
// delegates to currentUser (%USERNAME%) so defaultProvider behaves the same
// way on every platform.
func currentUserName() string {
	return currentUser()
}

// narrowToAccessible is a no-op on Windows: there's no systemd -M cross-user
// enumeration gap the way Linux has (see discover_unix.go), so the ghost-
// provider permission case doesn't apply here.
func narrowToAccessible(providers []Provider) []Provider {
	return providers
}
