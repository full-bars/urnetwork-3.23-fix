//go:build windows

package urnettools

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// listProviderProcesses enumerates every live provider process with the
// toolhelp snapshot, one entry per process (no dedup), with its owner.
// Unlike discoverProcesses it returns the snapshot error: uninstall must
// fail closed when it cannot see what is running.
func listProviderProcesses() ([]providerProcess, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("process snapshot: %w", err)
	}
	defer windows.CloseHandle(snapshot)

	var out []providerProcess
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	err = windows.Process32First(snapshot, &entry)
	for err == nil {
		name := windows.UTF16ToString(entry.ExeFile[:])
		if isProviderArg(name) {
			known, mine := processOwnedByCaller(entry.ProcessID)
			out = append(out, providerProcess{
				PID:           int(entry.ProcessID),
				Exe:           resolveProcessExe(int(entry.ProcessID), name),
				OwnerKnown:    known,
				OwnedByCaller: mine,
			})
		}
		err = windows.Process32Next(snapshot, &entry)
	}
	if err != windows.ERROR_NO_MORE_FILES {
		return nil, fmt.Errorf("process snapshot walk: %w", err)
	}
	return out, nil
}

// processOwnedByCaller reads the owner SID of a process and compares it with
// the invoking user's. known is false when the owner cannot be read.
func processOwnedByCaller(pid uint32) (known, mine bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false, false
	}
	defer windows.CloseHandle(h)
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return false, false
	}
	defer tok.Close()
	owner, err := tok.GetTokenUser()
	if err != nil {
		return false, false
	}
	me, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false, false
	}
	return true, owner.User.Sid.Equals(me.User.Sid)
}
