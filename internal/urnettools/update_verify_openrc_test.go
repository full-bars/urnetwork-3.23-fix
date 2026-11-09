package urnettools

import (
	"os"
	"runtime"
	"testing"
)

// nativeExecutableMagic returns a header that isRecognizedExecutable accepts
// on the running OS: ELF on linux and the BSDs, Mach-O on darwin, PE on
// windows.
func nativeExecutableMagic() []byte {
	switch runtime.GOOS {
	case "darwin":
		return []byte{0xcf, 0xfa, 0xed, 0xfe, 0, 0, 0, 0}
	case "windows":
		return []byte("MZ\x90\x00\x03\x00\x00\x00")
	default:
		return []byte("\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00")
	}
}

// Under OpenRC the running image can be a file the service user influenced,
// so the verify loop must read its version without running it. The
// exec-capable resolver must never be called there, and an image with no
// readable stamp fails closed. Every other supervisor keeps the exec-capable
// resolver, because the release build strips build info.
func TestUpdateVerification_OpenRCNeverCallsExecResolver(t *testing.T) {
	const tag = "v3.23.0-fix.35.9"
	// The read-only resolver only scans files that start with the running
	// OS's native executable magic, so the fixtures do too. A fixed ELF header
	// would be refused on Windows and macOS before the stamp is ever read.
	native := nativeExecutableMagic()
	stamped := append(append([]byte{}, native...), []byte("\n"+versionStampPrefix+tag+"\n")...)
	unstamped := append(append([]byte{}, native...), []byte("\nno stamp here\n")...)
	script := []byte("#!/bin/sh\necho " + tag + "\n")

	for _, tc := range []struct {
		name       string
		image      []byte
		supervisor string
		viaFn      bool
		wantOK     bool
		wantExec   bool
	}{
		{name: "openrc field, stamped image verifies", image: stamped, supervisor: "openrc", wantOK: true},
		{name: "openrc detected from the process, stamped image verifies", image: stamped, viaFn: true, wantOK: true},
		{name: "openrc field, unstamped image fails closed", image: unstamped, supervisor: "openrc"},
		{name: "openrc detected from the process, unstamped image fails closed", image: unstamped, viaFn: true},
		{name: "other supervisor keeps the exec-capable resolver", image: script, wantOK: true, wantExec: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubVerifyLoop(t)
			image := t.TempDir() + "/urnetwork"
			if err := os.WriteFile(image, tc.image, 0o755); err != nil {
				t.Fatal(err)
			}
			oldSup := providerSupervisedByOpenRCFn
			t.Cleanup(func() { providerSupervisedByOpenRCFn = oldSup })
			providerSupervisedByOpenRCFn = func(Provider) bool { return tc.viaFn }

			p := testProvider("/home/user/.urnetwork", 100)
			cfg := testConfig(tag)
			verifyDiscoverFn = func() []Provider {
				return []Provider{{StateDir: p.StateDir, PID: 200, Supervisor: tc.supervisor}}
			}
			verifyRunningImageHandleFn = func(int) (string, error) { return image, nil }
			execCalled := false
			verifyProviderVersionFn = func(string) string {
				execCalled = true
				return tag
			}

			var err error
			captureStdout(t, func() { err = verifyRestartLoop(p, cfg, false, "") })
			if (err == nil) != tc.wantOK {
				t.Fatalf("verify err=%v, want ok=%v", err, tc.wantOK)
			}
			if execCalled != tc.wantExec {
				t.Fatalf("exec-capable resolver called=%v, want %v", execCalled, tc.wantExec)
			}
		})
	}
}
