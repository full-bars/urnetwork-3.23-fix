package urnettools

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestExtenderSysctlPreservesExistingDropin(t *testing.T) {
	old := []byte("# operator setting\nnet.ipv4.ip_unprivileged_port_start=80\n")
	var commands []string
	var written bool
	cfg := &SystemPlatformConfigurer{PlatformName: "linux", Route: "sysctl", ReadFile: func(p string) ([]byte, error) {
		if p == extenderSysctlDropin {
			return old, nil
		}
		if p == "/proc/sys/net/ipv4/ip_unprivileged_port_start" {
			if written {
				return []byte("443"), nil
			}
			return []byte("80"), nil
		}
		return nil, os.ErrNotExist
	},
		Lstat: func(p string) (os.FileInfo, error) {
			if p == extenderSysctlDropin {
				return fakeSysctlFileInfo{mode: 0o600}, nil
			}
			return nil, os.ErrNotExist
		},
		WriteFile: func(p string, b []byte, m os.FileMode) error {
			if p != extenderSysctlDropin {
				t.Fatalf("unexpected write %s", p)
			}
			written = true
			return nil
		},
		ExecCmd: func(n string, a ...string) ([]byte, error) {
			commands = append(commands, setupCommand{n, a}.String())
			return nil, nil
		}}
	_, desc, err := cfg.ApplyRequirement("/provider", "provider")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(commands, ";"), "cp -p") || !strings.Contains(desc, "sudo mv "+extenderSysctlDropin+".pre-urnetwork "+extenderSysctlDropin) || strings.Contains(desc, "sudo rm "+extenderSysctlDropin) {
		t.Fatalf("commands=%v recovery=%s", commands, desc)
	}
}

type fakeSysctlFileInfo struct{ mode os.FileMode }

func (f fakeSysctlFileInfo) Name() string           { return "dropin" }
func (f fakeSysctlFileInfo) Size() int64            { return 0 }
func (f fakeSysctlFileInfo) Mode() os.FileMode      { return f.mode }
func (f fakeSysctlFileInfo) ModTime() (v time.Time) { return }
func (f fakeSysctlFileInfo) IsDir() bool            { return false }
func (f fakeSysctlFileInfo) Sys() any               { return nil }

func TestExtenderSysctlRequiresPersistence(t *testing.T) {
	cfg := &SystemPlatformConfigurer{PlatformName: "linux", Route: "sysctl", ReadFile: func(p string) ([]byte, error) {
		if p == "/proc/sys/net/ipv4/ip_unprivileged_port_start" {
			return []byte("443"), nil
		}
		return nil, os.ErrNotExist
	}}
	ok, desc, err := cfg.CheckRequirement("/provider", "provider")
	if err != nil || ok || !strings.Contains(desc, "not persisted") {
		t.Fatalf("ok=%v desc=%s err=%v", ok, desc, err)
	}
	cfg.ReadFile = func(p string) ([]byte, error) {
		if p == "/proc/sys/net/ipv4/ip_unprivileged_port_start" {
			return []byte("443"), nil
		}
		if p == "/etc/sysctl.conf" {
			return []byte("# ignored\nnet.ipv4.ip_unprivileged_port_start = 80\n"), nil
		}
		return nil, os.ErrNotExist
	}
	ok, _, err = cfg.CheckRequirement("/provider", "provider")
	if err != nil || !ok {
		t.Fatalf("persisted setting rejected: ok=%v err=%v", ok, err)
	}
	cfg.ReadFile = func(p string) ([]byte, error) {
		if p == "/proc/sys/net/ipv4/ip_unprivileged_port_start" {
			return []byte("443"), nil
		}
		if p == "/etc/sysctl.conf" {
			return []byte("net.ipv4.ip_unprivileged_port_start=80\nnet.ipv4.ip_unprivileged_port_start=1024\n"), nil
		}
		return nil, os.ErrNotExist
	}
	ok, _, _ = cfg.CheckRequirement("/provider", "provider")
	if ok {
		t.Fatal("conflicting persistent high value accepted")
	}
}

func TestExtenderSysctlBackupFailureDoesNotWrite(t *testing.T) {
	written := false
	cfg := &SystemPlatformConfigurer{PlatformName: "linux", Route: "sysctl", Lstat: func(p string) (os.FileInfo, error) {
		if p == extenderSysctlDropin {
			return fakeSysctlFileInfo{mode: 0o600}, nil
		}
		return nil, os.ErrNotExist
	}, ReadFile: func(p string) ([]byte, error) {
		if p == "/proc/sys/net/ipv4/ip_unprivileged_port_start" {
			return []byte("1024"), nil
		}
		return nil, os.ErrNotExist
	}, WriteFile: func(string, []byte, os.FileMode) error { written = true; return nil }, ExecCmd: func(n string, a ...string) ([]byte, error) {
		if n != "cp" {
			t.Fatalf("unexpected command %s", n)
		}
		return []byte("denied"), os.ErrPermission
	}}
	_, _, err := cfg.ApplyRequirement("/provider", "provider")
	if err == nil || !strings.Contains(err.Error(), "backing up") || written {
		t.Fatalf("err=%v wrote=%v", err, written)
	}
}
