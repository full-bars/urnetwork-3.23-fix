//go:build windows

package urnettools

import (
	"os"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// openAppendShared opens path for appending, creating it if needed, with
// FILE_SHARE_DELETE so the file can still be moved or replaced while the handle
// is open. os.OpenFile does not set that flag on Windows.
func openAppendShared(t *testing.T, path string) *os.File {
	t.Helper()
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(
		name,
		syscall.FILE_APPEND_DATA,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	return os.NewFile(uintptr(h), path)
}

// fileRenameInfo is FILE_RENAME_INFO with the Flags member of the Ex variant.
type fileRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

const (
	fileRenameFlagReplaceIfExists = 0x1
	fileRenameFlagPosixSemantics  = 0x2
)

// replaceFile puts replacement at path in one atomic step, as a rename does on
// Linux, while other handles may have path open. os.Rename on Windows fails
// with access denied in that case; the POSIX-semantics rename (Windows 10 1607
// and later, NTFS) succeeds when every open handle allows FILE_SHARE_DELETE.
func replaceFile(t *testing.T, replacement, path string) {
	t.Helper()
	src, err := windows.UTF16PtrFromString(replacement)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(
		src,
		windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)

	dst, err := windows.UTF16FromString(path)
	if err != nil {
		t.Fatal(err)
	}
	nameChars := dst[:len(dst)-1]
	nameBytes := len(nameChars) * 2
	size := int(unsafe.Offsetof(fileRenameInfo{}.FileName)) + nameBytes
	buf := make([]uint64, (size+7)/8)
	info := (*fileRenameInfo)(unsafe.Pointer(&buf[0]))
	info.Flags = fileRenameFlagReplaceIfExists | fileRenameFlagPosixSemantics
	info.FileNameLength = uint32(nameBytes)
	copy(unsafe.Slice(&info.FileName[0], len(nameChars)), nameChars)
	if err := windows.SetFileInformationByHandle(h, windows.FileRenameInfoEx, (*byte)(unsafe.Pointer(info)), uint32(size)); err != nil {
		t.Fatalf("replace %s with %s: %v", path, replacement, err)
	}
}
