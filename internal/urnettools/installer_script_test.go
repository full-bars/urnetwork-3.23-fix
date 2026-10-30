package urnettools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The do_install body was once pasted twice into Provider_Install_Linux.sh, so
// every install downloaded and installed everything two times. These markers
// each belong to exactly one step of do_install; a second copy trips the count.
func TestInstallerScriptDoInstallBodyNotDuplicated(t *testing.T) {
	b, err := os.ReadFile("../../scripts/Provider_Install_Linux.sh")
	if err != nil {
		t.Fatal(err)
	}
	// a Windows checkout may convert the script to CRLF; the markers span lines
	script := strings.ReplaceAll(string(b), "\r\n", "\n")
	for _, marker := range []string{
		`pr_info "Fetching release information for tag`,
		`ensure_tools_on_path "$install_path"`,
		`install_systemd_units
    fi`,
		`pr_info "Installation complete`,
	} {
		if n := strings.Count(script, marker); n != 1 {
			t.Errorf("marker %q appears %d times in Provider_Install_Linux.sh, want exactly 1", marker, n)
		}
	}
}

// resolve_service_user is the only thing deciding which account the FreeBSD
// provider runs as, and getting it wrong means the internet-facing relay runs
// as root. These cases EXECUTE the real function from the script (extracted
// verbatim, not re-implemented) with each precedence branch.
func TestFreeBSDResolveServiceUserPrecedence(t *testing.T) {
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")

	// Extract the function verbatim: from its definition to the closing brace
	// at column zero, so the test exercises shipped logic, not a copy of it.
	start := strings.Index(script, "resolve_service_user() {")
	if start < 0 {
		t.Fatal("resolve_service_user is missing from the FreeBSD installer")
	}
	rest := script[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of resolve_service_user")
	}
	fn := rest[:end+2]

	for _, c := range []struct {
		name string
		env  []string
		want string
	}{
		{"explicit override wins", []string{"SERVICE_USER_OVERRIDE=bob"}, "bob"},
		{"sudo parent beats logname", []string{"SUDO_USER=alice"}, "alice"},
		{"override beats sudo", []string{"SERVICE_USER_OVERRIDE=bob", "SUDO_USER=alice"}, "bob"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// env is set on the command, not inherited from the test process,
			// so each branch is exercised deterministically.
			cmd := exec.Command("/bin/sh", "-c", fn+"; resolve_service_user")
			cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, c.env...)
			got, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("resolve_service_user failed: %v (%s)", err, got)
			}
			if strings.TrimSpace(string(got)) != c.want {
				t.Errorf("got %q, want %q", strings.TrimSpace(string(got)), c.want)
			}
		})
	}
}

// Without sudo or an override the answer is the login owner - and it must
// never come back as an empty string, which would render an rc.d script with
// an empty user= and silently start the provider as root.
func TestFreeBSDResolveServiceUserNeverEmpty(t *testing.T) {
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")
	// Guarded extraction: a raw Index would slice from -1 and panic the whole
	// package run if the function is renamed, instead of failing this test.
	fn := extractShellFunc(t, script, "resolve_service_user")
	if fn == "" {
		t.Fatal("could not extract resolve_service_user from the FreeBSD installer")
	}

	cmd := exec.Command("/bin/sh", "-c", fn+"; resolve_service_user")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	got, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve_service_user failed: %v (%s)", err, got)
	}
	resolved := strings.TrimSpace(string(got))
	if resolved == "" {
		t.Fatal("resolve_service_user returned empty; the rc script would render an empty user")
	}
	// With no sudo parent, no override and no login tty, the answer must be
	// this process's own user (the `id -un` step). An earlier version asserted
	// only non-emptiness, which `id -un` cannot violate, so the test could
	// never fail (mutation-proved: deleting the logname branch did not matter,
	// and neither would mis-resolving the fallthrough).
	want, err := exec.Command("id", "-un").Output()
	if err != nil {
		t.Fatalf("id -un: %v", err)
	}
	if resolved != strings.TrimSpace(string(want)) {
		t.Errorf("resolve_service_user = %q, want the current user %q", resolved, strings.TrimSpace(string(want)))
	}
}

// Updating a running provider must never copy onto the executing image:
// ETXTBSY makes `update` fail on a live box. Stage and rename instead.
func TestFreeBSDInstallerNeverCopiesOntoRunningBinary(t *testing.T) {
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")

	body := extractShellFunc(t, script, "do_install")
	if body == "" {
		t.Fatal("could not extract do_install from the installer")
	}
	// Executable lines only: a commented-out `mv -f` satisfied the old
	// string check while the live code copied (mutation-proved).
	execLines := shellExecLines(body)

	stagedRename, refusalIdx, firstFetch := -1, -1, -1
	for i, line := range execLines {
		switch {
		case strings.Contains(line, `mv -f "$staged" "$provider_bin"`):
			stagedRename = i
		case strings.Contains(line, `if [ "$service_user" = "root" ]; then`):
			if refusalIdx < 0 {
				refusalIdx = i
			}
		case strings.Contains(line, "curl -fsSL") && firstFetch < 0:
			firstFetch = i
		}
		// Any copy whose destination is the live binary is the ETXTBSY bug,
		// whatever flags it carries (`cp -f`, `cp -p`, quoted variants).
		if strings.Contains(line, "cp ") && strings.Contains(line, `"$provider_bin"`) {
			t.Errorf("do_install copies onto the live binary (ETXTBSY on update): %q", line)
		}
	}
	if stagedRename < 0 {
		t.Error("do_install does not rename the staged binary into place")
	}
	// The refusal must be INSIDE do_install and BEFORE any download. Checking
	// the whole file passed even when the refusal was deleted from do_install,
	// because do_install_service carries the same line (mutation-proved).
	if refusalIdx < 0 {
		t.Error("do_install has no refusal for a root service user")
	} else if firstFetch >= 0 && refusalIdx > firstFetch {
		t.Error("do_install downloads before refusing a root service user; the refusal must come first")
	}
	// The install tree stays root-owned: the rc.d start runs as root before
	// daemon(8) drops to the service user, and the Linux installer chowns its
	// tree to root for the same reason. What must belong to the service user
	// is its state directory, where its config and JWT live. The repair itself
	// moved into enforce_install_tree_perms, which is a checked gate (see
	// TestFreeBSDInstallTreePermsGate*), so what do_install must still do is
	// route through it before the rc.d script is written.
	if !strings.Contains(body, `enforce_install_tree_perms "$install_path" "$staged"`) {
		t.Error("do_install does not enforce the install tree's ownership and traversal")
	}
	// The chown runs on '.' inside a `cd "$state_dir"` subshell. -h is main's
	// no-follow form: a symlink that appears under the state dir must not be
	// dereferenced by a recursive chown. The cd pins the directory the chown
	// actually operates on, so a symlink swapped in at state_dir after the
	// check above cannot redirect it; dropping the cd must fail this test.
	stateOwned := strings.Contains(body, `cd "$state_dir"`) &&
		strings.Contains(body, `chown -R -h "$service_user" .`)
	if !stateOwned {
		t.Error(`do_install does not hand the state directory to the service user (want cd "$state_dir" plus chown -R -h "$service_user" .)`)
	}
	// The service must not be started when a failure to start would be
	// reported as success: the start/restart outcome is checked.
	if !strings.Contains(body, "Failed to start the service") {
		t.Error("do_install does not fail when the service fails to start")
	}
}

// write_state_flag() writes into a directory the service user owns, as root
// under sudo. A check-then-write on that path is unwinnable: the service user
// can swap state_dir for a symlink, or plant one at the flag name. These cases
// execute the shipped function and pin the fix: the write enters the directory
// and renames over the flag entry (rename does not follow it), and a symlinked
// state_dir is refused outright.
func TestFreeBSDWriteStateFlagDoesNotFollowSymlinks(t *testing.T) {
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")
	fn := extractShellFunc(t, script, "write_state_flag")
	if fn == "" {
		t.Fatal("could not extract write_state_flag from the FreeBSD installer")
	}

	dir := t.TempDir()
	harness := filepath.Join(dir, "flag.sh")
	body := "pr_err() { printf 'ERR: %s\\n' \"$1\" >&2; }\n" +
		"state_dir=\"$1\"\nservice_user=\"$(id -un)\"\n" +
		fn + "\nwrite_state_flag \"$2\" \"$3\"\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(stateDir, name, value string) error {
		out, err := exec.Command("/bin/sh", harness, stateDir, name, value).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	}

	t.Run("symlink at the flag name is replaced, not followed", func(t *testing.T) {
		stateDir := filepath.Join(dir, "state-flag")
		if err := os.Mkdir(stateDir, 0o755); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(dir, "victim")
		if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		flag := filepath.Join(stateDir, "hot_restart")
		if err := os.Symlink(victim, flag); err != nil {
			t.Fatal(err)
		}
		if err := run(stateDir, "hot_restart", "on"); err != nil {
			t.Fatalf("write_state_flag: %v", err)
		}
		if got, err := os.ReadFile(victim); err != nil || string(got) != "keep" {
			t.Fatalf("write followed the symlink into the victim: %q, %v", got, err)
		}
		fi, err := os.Lstat(flag)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			t.Error("flag is still a symlink; the write did not replace the entry")
		}
		if got, err := os.ReadFile(flag); err != nil || strings.TrimSpace(string(got)) != "on" {
			t.Errorf("flag = %q, %v; want on", got, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o644 {
			t.Errorf("flag mode = %o; want 644 (umask 022 at creation, no chmod)", perm)
		}
	})

	t.Run("symlinked state dir is refused", func(t *testing.T) {
		target := filepath.Join(dir, "target")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		stateDir := filepath.Join(dir, "state-link")
		if err := os.Symlink(target, stateDir); err != nil {
			t.Fatal(err)
		}
		if err := run(stateDir, "hot_restart", "on"); err == nil {
			t.Fatal("write_state_flag wrote through a symlinked state_dir")
		}
		if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
			t.Fatalf("target of the symlinked state dir changed: %v, %v", entries, err)
		}
	})
}

// The install tree's ownership and traversal repair used to be best effort:
// `chown ... || chown ... || true`, `chmod -R go-w ... || true` and
// `chmod 755 ... || true`. do_install then wrote the rc.d script and reported
// "installed" for a tree the service user cannot traverse or execute. These
// cases run the shipped gate with each repair failing in turn, and each one
// asserts the MESSAGE that names the path and the failure: a bare exit status
// leaves the operator with the same silence the fix removes.
func TestFreeBSDInstallTreePermsGateAborts(t *testing.T) {
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")
	fn := extractShellFunc(t, script, "enforce_install_tree_perms")
	if fn == "" {
		t.Fatal("could not extract enforce_install_tree_perms from the FreeBSD installer")
	}

	// The harness stubs the two programs the gate drives, because a real chown
	// needs root and only one repair may fail per case. chmod wraps the real
	// one so the success case leaves a genuinely traversable tree; everything
	// between is the function lifted verbatim out of the installer.
	harness := `priv() { "$@"; }
pr_err() {
    fmt="$1"; shift
    printf 'ERR: '"$fmt"'\n' "$@" >&2
}
pr_info() {
    fmt="$1"; shift
    printf 'INFO: '"$fmt"'\n' "$@"
}
stat() {
    [ "${STAT_FAIL:-0}" = 1 ] && return 1
    if [ "$1" = -f ]; then
        case "$2" in
            %u) [ "${STAT_ROOT:-0}" = 1 ] && { printf '0\n'; return; }; /usr/bin/stat -c %u "$3" 2>/dev/null || /usr/bin/stat -f %u "$3" ;;
            %Lp) /usr/bin/stat -c %a "$3" 2>/dev/null || /usr/bin/stat -f %Lp "$3" ;;
            *) return 1 ;;
        esac
        return
    fi
    /usr/bin/stat "$@"
}
chown() {
    case "${FAIL_CHOWN:-none}" in
        all) return 1 ;;
        group) case "$2" in root:wheel) return 1 ;; esac ;;
    esac
    return 0
}
chmod() {
    case "$*" in
        "-R go-w "*) [ "${FAIL_GOW:-0}" = 1 ] && return 1 ;;
    esac
    [ "$1" = 755 ] || { /bin/chmod "$@"; return; }
    case "${FAIL_755:-none}" in
        all) return 1 ;;
        tree) [ "$2" = "$TREE" ] && return 1 ;;
        bin) [ "$2" = "$TREE/bin" ] && return 1 ;;
        binary) [ "$2" = "$BIN" ] && return 1 ;;
    esac
    /bin/chmod "$@"
}
` + fn + `
enforce_install_tree_perms "$TREE" "$BIN"
`

	// A tree in the state a restrictive umask leaves behind: 0700 directories
	// and a 0644 image, which daemon -u can neither traverse nor exec.
	setup := func(t *testing.T) (tree, bin string) {
		t.Helper()
		tree = filepath.Join(t.TempDir(), "urnetwork-provider")
		binDir := filepath.Join(tree, "bin")
		if err := os.MkdirAll(binDir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{tree, binDir} {
			if err := os.Chmod(p, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		bin = filepath.Join(binDir, "urnetwork")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return tree, bin
	}
	run := func(t *testing.T, tree, bin string, env ...string) (string, error) {
		t.Helper()
		h := filepath.Join(t.TempDir(), "gate.sh")
		if err := os.WriteFile(h, []byte(harness), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh", h)
		cmd.Env = append([]string{"PATH=/usr/bin:/bin", "TREE=" + tree, "BIN=" + bin}, env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("failed chown aborts and names the tree", func(t *testing.T) {
		tree, bin := setup(t)
		out, err := run(t, tree, bin, "FAIL_CHOWN=all")
		if err == nil {
			t.Fatalf("gate passed with both chowns failing; the install would report success for a tree root does not own (%s)", out)
		}
		want := "ERR: could not make " + tree + " root-owned: chown -R root:wheel and chown -R root both failed"
		if !strings.Contains(out, want) {
			t.Errorf("the abort does not name the path and the failure\n want: %s\n  got: %s", want, out)
		}
	})

	t.Run("failed chown continues when tree already conforms", func(t *testing.T) {
		tree, bin := setup(t)
		for _, p := range []string{tree, filepath.Join(tree, "bin"), bin} {
			if err := os.Chmod(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		out, err := run(t, tree, bin, "FAIL_CHOWN=all", "STAT_ROOT=1")
		if err != nil {
			t.Fatalf("gate aborted although the tree already conforms: %v (%s)", err, out)
		}
		want := "INFO: chown -R root:wheel and chown -R root both failed; " + tree + " already conformed"
		if !strings.Contains(out, want) {
			t.Fatalf("missing already-conformed diagnostic\n want: %s\n  got: %s", want, out)
		}
	})

	t.Run("undetermined ownership aborts even after successful repairs", func(t *testing.T) {
		tree, bin := setup(t)
		out, err := run(t, tree, bin, "STAT_FAIL=1")
		if err == nil || !strings.Contains(out, "could not verify ownership and permissions") {
			t.Fatalf("gate must refuse when ownership cannot be determined: %v (%s)", err, out)
		}
	})

	t.Run("failed root:wheel chown falls back to root", func(t *testing.T) {
		tree, bin := setup(t)
		out, err := run(t, tree, bin, "FAIL_CHOWN=group", "STAT_ROOT=1")
		if err != nil {
			t.Fatalf("gate aborted although chown -R root succeeds: %v (%s)", err, out)
		}
		if strings.Contains(out, "ERR:") {
			t.Errorf("a recoverable chown reported an error: %s", out)
		}
	})

	// The fallback accepts a tree that already conforms, so the conformance check
	// has to cover every regular file root runs, not only the directories and the
	// daemon binary. bin/urnet-tools is root-executed and sits beside the daemon.
	t.Run("fallback rejects a group-writable sibling binary", func(t *testing.T) {
		tree, bin := setup(t)
		tool := filepath.Join(tree, "bin", "urnet-tools")
		if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o775); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(tool, 0o775); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{tree, filepath.Join(tree, "bin"), bin} {
			if err := os.Chmod(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		out, err := run(t, tree, bin, "FAIL_GOW=1", "STAT_ROOT=1")
		if err == nil {
			t.Fatalf("gate accepted a group-writable urnet-tools after chmod -R go-w failed (%s)", out)
		}
		want := "ERR: could not remove group/other write access from " + tree + ": chmod -R go-w failed"
		if !strings.Contains(out, want) {
			t.Errorf("the abort does not name the path and the failure\n want: %s\n  got: %s", want, out)
		}
	})

	t.Run("failed go-w chmod aborts and names the tree", func(t *testing.T) {
		tree, bin := setup(t)
		out, err := run(t, tree, bin, "FAIL_GOW=1")
		if err == nil {
			t.Fatalf("gate passed with the group/other write repair failing (%s)", out)
		}
		want := "ERR: could not remove group/other write access from " + tree + ": chmod -R go-w failed"
		if !strings.Contains(out, want) {
			t.Errorf("the abort does not name the path and the failure\n want: %s\n  got: %s", want, out)
		}
	})

	t.Run("failed tree traversal chmod aborts and names the tree", func(t *testing.T) {
		tree, bin := setup(t)
		out, err := run(t, tree, bin, "FAIL_755=tree")
		if err == nil {
			t.Fatalf("gate passed with the tree's chmod 755 failing (%s)", out)
		}
		want := "ERR: could not make " + tree + " traversable by the service user: chmod 755 failed"
		if !strings.Contains(out, want) {
			t.Errorf("the abort does not name the path and the failure\n want: %s\n  got: %s", want, out)
		}
	})

	// 0755 on bin/ is no use while the directory above it stays 0700, so the
	// two are separate repairs and the message has to say which one failed.
	t.Run("failed bin traversal chmod aborts and names bin", func(t *testing.T) {
		tree, bin := setup(t)
		out, err := run(t, tree, bin, "FAIL_755=bin")
		if err == nil {
			t.Fatalf("gate passed with bin's chmod 755 failing (%s)", out)
		}
		want := "ERR: could not make " + filepath.Join(tree, "bin") + " traversable by the service user: chmod 755 failed"
		if !strings.Contains(out, want) {
			t.Errorf("the abort does not name the path and the failure\n want: %s\n  got: %s", want, out)
		}
	})

	t.Run("failed binary chmod aborts and names the binary", func(t *testing.T) {
		tree, bin := setup(t)
		out, err := run(t, tree, bin, "FAIL_755=binary")
		if err == nil {
			t.Fatalf("gate passed with the binary's chmod 755 failing (%s)", out)
		}
		want := "ERR: could not make " + bin + " executable by the service user: chmod 755 failed"
		if !strings.Contains(out, want) {
			t.Errorf("the abort does not name the path and the failure\n want: %s\n  got: %s", want, out)
		}
	})

	// The install continues on the root path only when every repair landed, so
	// this pins the repaired shape rather than the absence of an error.
	t.Run("successful repair leaves the tree startable", func(t *testing.T) {
		tree, bin := setup(t)
		out, err := run(t, tree, bin, "STAT_ROOT=1")
		if err != nil {
			t.Fatalf("gate aborted with every repair succeeding: %v (%s)", err, out)
		}
		if strings.Contains(out, "ERR:") {
			t.Errorf("the gate reported an error on the success path: %s", out)
		}
		for _, p := range []string{tree, filepath.Join(tree, "bin"), bin} {
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != 0o755 {
				t.Errorf("%s mode = %o, want 755: the service user cannot traverse or execute it", p, got)
			}
		}
	})
}

func TestFreeBSDInstallTreePermsRejectsSymlinks(t *testing.T) {
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	fn := extractShellFunc(t, strings.ReplaceAll(string(b), "\r\n", "\n"), "enforce_install_tree_perms")
	if fn == "" {
		t.Fatal("could not extract enforce_install_tree_perms")
	}

	harness := `priv() { if [ "${FAIL_SCAN:-0}" = 1 ] && [ "$1" = find ]; then return 1; fi; "$@"; }
pr_err() { fmt="$1"; shift; printf 'ERR: '"$fmt"'\n' "$@" >&2; }
pr_info() { :; }
stat() { case "$2" in %u) printf '0\n' ;; %Lp) printf '755\n' ;; *) return 1 ;; esac; }
chown() { printf 'chown\n' >> "$REPAIRS"; [ "${FAIL_CHOWN:-0}" != 1 ]; }
chmod() { printf 'chmod\n' >> "$REPAIRS"; }
` + fn + `
enforce_install_tree_perms "$TREE" "$BIN"
`
	for _, c := range []struct {
		name, target        string
		failChown, failScan bool
	}{
		{"file repair succeeds", "urnetwork", false, false},
		{"directory repair succeeds", ".", false, false},
		{"dangling repair succeeds", "missing", false, false},
		{"file already conformed fallback", "urnetwork", true, false},
		{"scan failure", "", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree := filepath.Join(t.TempDir(), "tree")
			binDir := filepath.Join(tree, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(binDir, "urnetwork")
			if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
				t.Fatal(err)
			}
			wantPath := tree
			if !c.failScan {
				wantPath = filepath.Join(binDir, "link with spaces")
				if err := os.Symlink(c.target, wantPath); err != nil {
					t.Fatal(err)
				}
			}
			repairs := filepath.Join(t.TempDir(), "repairs")
			cmd := exec.Command("/bin/sh", "-c", harness)
			cmd.Env = []string{"PATH=/usr/bin:/bin", "TREE=" + tree, "BIN=" + bin, "REPAIRS=" + repairs}
			if c.failChown {
				cmd.Env = append(cmd.Env, "FAIL_CHOWN=1")
			}
			if c.failScan {
				cmd.Env = append(cmd.Env, "FAIL_SCAN=1")
			}
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Errorf("gate accepted %s: %s", wantPath, out)
			}
			if !strings.Contains(string(out), wantPath) || !strings.Contains(string(out), "ERR:") {
				t.Errorf("error must name %s: %s", wantPath, out)
			}
			if _, err := os.Stat(repairs); !os.IsNotExist(err) {
				t.Errorf("repair ran before symlink rejection: stat error %v", err)
			}
		})
	}
}

// A checked repair is only a gate if the install acts on it. These assertions
// pin the call site: do_install must call enforce_install_tree_perms, and a
// failing gate must exit before the rc.d script is written and before the
// success message, because "the install aborted" is what the operator needs to
// be true. The swallowed forms are refuted across the whole script, so
// restoring any one of them fails here rather than next review cycle.
func TestFreeBSDInstallTreePermsGateGatesTheInstall(t *testing.T) {
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")
	body := extractShellFunc(t, script, "do_install")
	if body == "" {
		t.Fatal("could not extract do_install from the installer")
	}

	gateIdx, exitIdx, rcIdx, doneIdx := -1, -1, -1, -1
	var gateLine string
	for i, line := range shellExecLines(body) {
		switch {
		case strings.HasPrefix(line, "if ! enforce_install_tree_perms"):
			if gateIdx < 0 {
				gateIdx, gateLine = i, line
			}
		case gateIdx >= 0 && exitIdx < 0 && line == "exit 1":
			exitIdx = i
		case strings.Contains(line, "write_rc_script_body") && rcIdx < 0:
			rcIdx = i
		case strings.Contains(line, "URnetwork provider %s installed") && doneIdx < 0:
			doneIdx = i
		}
	}
	if gateIdx < 0 {
		t.Fatal("do_install does not call enforce_install_tree_perms")
	}
	// Repairs must name this install's tree and binary: a bare `$install_path`
	// read from a global can be repointed by an earlier assignment.
	if !strings.Contains(gateLine, `"$install_path" "$staged"`) {
		t.Errorf("the gate is not called on the install tree and binary: %q", gateLine)
	}
	if exitIdx < 0 {
		t.Fatal("do_install calls the gate but does not abort when it fails")
	}
	if rcIdx < 0 {
		t.Fatal("do_install no longer writes the rc.d script; this test needs updating")
	}
	if gateIdx > rcIdx {
		t.Error("the gate runs after the rc.d script is written; a broken tree would already be installed as a service")
	}
	if exitIdx > rcIdx {
		t.Error("a failed gate does not abort before the rc.d script is written")
	}
	if doneIdx < 0 {
		t.Fatal("do_install has no success message; this test needs updating")
	}
	if exitIdx > doneIdx {
		t.Error("a failed gate can fall through to the success message")
	}
	for _, swallowed := range []string{
		`chown -R root:wheel "$install_path" 2>/dev/null ||`,
		`chown -R root "$install_path" 2>/dev/null || true`,
		`chmod -R go-w "$install_path" 2>/dev/null || true`,
		`chmod 755 "$install_path" "$install_path/bin" 2>/dev/null || true`,
	} {
		if strings.Contains(script, swallowed) {
			t.Errorf("installer still swallows the permission repair: %q", swallowed)
		}
	}
}

// Run the real install body with a failing permission gate and local release
// stubs. The fake uid exercises the non-root branch even on a root test host.
func freeBSDGateFailure(t *testing.T) (string, string, string) {
	t.Helper()
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	body := extractShellFunc(t, strings.ReplaceAll(string(b), "\r\n", "\n"), "do_install")
	if body == "" {
		t.Fatal("missing do_install")
	}
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]string{"bin/urnetwork": "old binary", "version": "old version"} {
		if err := os.WriteFile(filepath.Join(tree, path), []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	harness := `install_path="$1"
 provider_bin="$install_path/bin/urnetwork"
 state_dir="$install_path/state"
 service_user=testuser
 github_api=example
 me=test
 id() { if [ "$1" = -u ]; then printf '1000\n'; else command id "$@"; fi; }
 priv() { "$@"; }
 priv_rm() { [ -z "$1" ] || rm -f "$1"; }
 install_cleanup() { priv_rm "${staged_bin:-}"; priv_rm "${staged_tool:-}"; }
 assert_state_path_safe() { :; }
 pr_info() { :; }
 pr_warn() { :; }
 pr_err() { :; }
 http_fetch() { if [ "$#" -gt 1 ]; then printf 'tool' > "$2"; else printf '{"assets":[{"name":"urnet-tools-freebsd-amd64","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}'; fi; }
 jq() { case "$*" in *tag_name*) printf 'test-tag\n' ;; *) printf '%064d\n' 0 | tr 0 a ;; esac; }
 uname() { printf 'amd64\n'; }
 sha256() { printf '%064d\n' 0 | tr 0 a; }
 tar() { printf 'new binary' > "$4/provider"; }
 install_wrapper_tool() { :; }
 enforce_install_tree_perms() { printf '%s\n%s\n' "$(id -u)" "$2" > "$install_path/gate-called"; return 1; }
 ` + body + `
 do_install test-tag
 `
	h := filepath.Join(dir, "harness.sh")
	if err := os.WriteFile(h, []byte(harness), 0755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", h, tree).CombinedOutput()
	if err == nil {
		t.Fatalf("install succeeded despite failed gate: %s", out)
	}
	read := func(path string) string {
		b, err := os.ReadFile(filepath.Join(tree, path))
		if err != nil {
			t.Fatalf("read %s: %v; output: %s", path, err, out)
		}
		return string(b)
	}
	return read("gate-called"), read("bin/urnetwork"), read("version")
}

func TestFreeBSDNonRootInstallReachesPermissionGate(t *testing.T) {
	gate, _, _ := freeBSDGateFailure(t)
	if !strings.HasPrefix(gate, "1000\n") || !strings.Contains(gate, "/bin/.urnetwork.new.") {
		t.Fatalf("gate did not receive non-root staged binary: %q", gate)
	}
}

func TestFreeBSDGateFailurePreservesBinaryAndVersion(t *testing.T) {
	_, binary, version := freeBSDGateFailure(t)
	if binary != "old binary" || version != "old version" {
		t.Fatalf("gate failure changed installed binary/version: %q / %q", binary, version)
	}
}

// shellExecLines returns the executable lines of a shell function body,
// trimmed. A commented-out copy of a repair satisfies a whole-text Contains
// while doing nothing, so assertions about what the installer DOES read only
// these.
func shellExecLines(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

// extractShellFunc returns the body of a top-level shell function, from its
// definition line to the closing brace at column 0. Used so assertions can
// target ONE function instead of the whole file, which is how a string present
// in a sibling function satisfied a check about do_install.
// extractShellFunc returns the body of a top-level shell function, from its
// definition line to the closing brace at column 0. Used so assertions can
// target ONE function instead of the whole file, which is how a string present
// in a sibling function satisfied a check about do_install.
func extractShellFunc(t *testing.T, script, name string) string {
	t.Helper()
	// The script declares functions in two styles: `name() {` and
	// `name ()` with the brace on the following line. Match either and take
	// the earliest, so a rename-vs-decl drift cannot silently empty the body.
	start := -1
	for _, cand := range []string{name + "()", name + " ()"} {
		if i := strings.Index(script, cand); i >= 0 && (start < 0 || i < start) {
			start = i
		}
	}
	if start < 0 {
		return ""
	}
	rest := script[start:]
	lines := strings.Split(rest, "\n")
	var heredocTag string
	endIdx := -1
	charCount := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if heredocTag != "" {
			if trimmed == heredocTag {
				heredocTag = ""
			}
		} else {
			if idx := strings.Index(line, "<<"); idx >= 0 && !strings.HasPrefix(trimmed, "#") {
				rem := line[idx+2:]
				if !strings.HasPrefix(rem, "<") { // ignore here-strings <<<
					rem = strings.TrimPrefix(rem, "-")
					rem = strings.TrimSpace(rem)
					var tag string
					if strings.HasPrefix(rem, "'") {
						if closeIdx := strings.Index(rem[1:], "'"); closeIdx >= 0 {
							tag = rem[1 : 1+closeIdx]
						}
					} else if strings.HasPrefix(rem, "\"") {
						if closeIdx := strings.Index(rem[1:], "\""); closeIdx >= 0 {
							tag = rem[1 : 1+closeIdx]
						}
					} else {
						end := strings.IndexAny(rem, " \t;<>|&)")
						if end >= 0 {
							tag = rem[:end]
						} else {
							tag = rem
						}
					}
					tag = strings.Trim(tag, "'\"\\")
					if tag != "" {
						heredocTag = tag
					}
				}
			} else if i > 0 && line == "}" {
				endIdx = charCount + len(line)
				break
			}
		}
		charCount += len(line) + 1
	}
	if endIdx < 0 {
		return ""
	}
	return rest[:endIdx]
}

// A release asset with no digest must read as ABSENT. jq prints the string
// "null" for a missing or null field, which passed the non-empty test and then
// failed the comparison, so every install fell back to the shell wrapper while
// reporting success. These cases run the real expression out of the shipped
// installer against the JSON shapes the releases API actually returns.
func TestFreeBSDToolDigestLookup(t *testing.T) {
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")
	block := extractShellFunc(t, script, "get_asset_digest_from_api_response")
	if block == "" || !strings.Contains(script, `tool_digest="$(get_asset_digest_from_api_response "$release_json" "$tool_asset")"`) {
		t.Fatal("tool digest lookup does not use the shared helper")
	}

	const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name string
		json string
		want string
	}{
		{"digest field absent", `{"assets":[{"name":"urnet-tools-freebsd-amd64"}]}`, ""},
		{"digest explicitly null", `{"assets":[{"name":"urnet-tools-freebsd-amd64","digest":null}]}`, ""},
		{"digest empty string", `{"assets":[{"name":"urnet-tools-freebsd-amd64","digest":""}]}`, ""},
		{"not a hex digest", `{"assets":[{"name":"urnet-tools-freebsd-amd64","digest":"deadbeef"}]}`, ""},
		{"different algorithm", `{"assets":[{"name":"urnet-tools-freebsd-amd64","digest":"sha512:` + hex64 + `"}]}`, ""},
		{"other asset matched", `{"assets":[{"name":"urnet-tools-linux-amd64","digest":"sha256:` + hex64 + `"}]}`, ""},
		{"real sha256", `{"assets":[{"name":"urnet-tools-freebsd-amd64","digest":"sha256:` + hex64 + `"}]}`, hex64},
		{"bare hex", `{"assets":[{"name":"urnet-tools-freebsd-amd64","digest":"` + hex64 + `"}]}`, hex64},
	}

	// jq and python3 are PACKAGES on FreeBSD, not base system: the FreeBSD CI
	// VM has neither unless installed. Skip a path whose tool is absent (and
	// skip the whole test when both are) rather than failing on the host's
	// package set — but never skip SILENTLY for a missing marker, which is a
	// real drift and must fail.
	haveJq := false
	if _, err := exec.LookPath("jq"); err == nil {
		haveJq = true
	}
	havePy := false
	if _, err := exec.LookPath("python3"); err == nil {
		havePy = true
	}
	if !haveJq && !havePy {
		t.Skip("neither jq nor python3 on this host; cannot execute the digest expressions")
	}

	// jq path: the expression is embedded in single quotes in the shell.
	// A missing marker is a FAILURE, not a skip: the previous version wrapped
	// this in `if found {`, so adding one space to the expression made every
	// jq subtest vanish while the test reported success (mutation-proved).
	jqStart := strings.Index(block, "jq -r --arg a \"$_ast\" '")
	if !haveJq {
		t.Log("jq not installed here; exercising only the python path")
	} else if jqStart < 0 {
		t.Error("could not find the jq digest expression in the installer; the marker moved and this test would otherwise pass vacuously")
	} else {
		expr := block[jqStart+len("jq -r --arg a \"$_ast\" '"):]
		if end := strings.Index(expr, "' 2>/dev/null"); end >= 0 {
			expr = expr[:end]
			for _, c := range cases {
				t.Run("jq/"+c.name, func(t *testing.T) {
					// jq reads the release JSON on stdin, so it must be wired:
					// Output() alone would leave it with no input and it would
					// quietly return nothing for every case.
					cmd := exec.Command("jq", "-r", "--arg", "a", "urnet-tools-freebsd-amd64", expr)
					cmd.Stdin = strings.NewReader(c.json)
					out, err := cmd.Output()
					if err != nil {
						t.Fatalf("jq failed: %v", err)
					}
					if got := strings.TrimSpace(string(out)); got != c.want {
						t.Errorf("got %q, want %q", got, c.want)
					}
				})
			}
		}
	}

	// python path; same rule: a missing marker fails rather than skips.
	pyStart := strings.Index(block, "python3 -c '")
	if !havePy {
		t.Log("python3 not installed here; exercising only the jq path")
	} else if pyStart < 0 {
		t.Error("could not find the python digest expression in the installer; the marker moved and this test would otherwise pass vacuously")
	} else {
		py := block[pyStart+len("python3 -c '"):]
		pyEnd := strings.Index(py, "' \"$_ast\"")
		if pyEnd < 0 {
			t.Error("could not find the end of the python digest expression; the marker moved")
		} else {
			py = py[:pyEnd]
			for _, c := range cases {
				t.Run("py/"+c.name, func(t *testing.T) {
					cmd := exec.Command("python3", "-c", py, "urnet-tools-freebsd-amd64")
					cmd.Stdin = strings.NewReader(c.json)
					out, err := cmd.Output()
					if err != nil {
						t.Fatalf("python failed: %v", err)
					}
					if got := strings.TrimSpace(string(out)); got != c.want {
						t.Errorf("got %q, want %q", got, c.want)
					}
				})
			}
		}
	}
}

// The release metadata path must work on a clean FreeBSD host: jq and python3
// are packages there, and this installer never installs packages. The base
// fallback is exercised by pointing PATH at a directory that holds only base
// tools, so `command -v jq` and `command -v python3` fail exactly as they do
// on a base-only box. A parser-dependent harness would skip here instead of
// proving the install still verifies.
func TestFreeBSDDigestAndAssetListOnBaseToolsOnly(t *testing.T) {
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")
	digestFn := extractShellFunc(t, script, "get_asset_digest_from_api_response")
	listFn := extractShellFunc(t, script, "release_lists_asset")
	scanFn := extractShellFunc(t, script, "release_asset_scan")
	if digestFn == "" || listFn == "" || scanFn == "" {
		t.Fatal("could not extract the FreeBSD digest, asset-list and base-scan helpers")
	}

	// A base-only PATH: no jq, no python3, nothing the fallback may lean on
	// beyond what FreeBSD ships. Resolve from the system directories rather
	// than LookPath: installer tests earlier in this package prepend stub
	// directories to PATH, and a stub that their cleanup already removed
	// would leave a dangling symlink and a 127 in the child shell.
	bin := t.TempDir()
	for _, tool := range []string{"tr", "awk", "sed", "grep", "cat"} {
		path := ""
		for _, dir := range []string{"/bin", "/usr/bin", "/usr/local/bin", "/sbin", "/usr/sbin"} {
			candidate := filepath.Join(dir, tool)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				path = candidate
				break
			}
		}
		if path == "" {
			t.Skipf("no %s in the system directories to build a base-only PATH", tool)
		}
		if err := os.Symlink(path, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	basePath := []string{"PATH=" + bin}

	const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// Pretty-printed like the GitHub API, which is what the installer actually
	// receives — the digest is not on the same line as the name.
	release := `{
  "assets": [
    {
      "name": "urnetwork-provider-v1-freebsd-amd64.tar.gz",
      "size": 123,
      "digest": "sha256:` + hex64 + `",
      "browser_download_url": "https://example.invalid/urnetwork-provider-v1-freebsd-amd64.tar.gz"
    },
    {
      "name": "other.txt",
      "digest": "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
    }
  ]
}`
	basePath = append(basePath, "RELEASE="+release, "ASSET=urnetwork-provider-v1-freebsd-amd64.tar.gz")

	run := func(expr string) (string, error) {
		cmd := exec.Command("/bin/sh", "-c", scanFn+"\n"+digestFn+"\n"+listFn+"\n"+expr)
		cmd.Env = basePath
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	if got, err := run(`command -v jq || command -v python3 || echo none`); err != nil || got != "none" {
		t.Fatalf("base-only PATH still has a parser (%q, %v); the fallback is not being exercised", got, err)
	}
	if got, err := run(`get_asset_digest_from_api_response "$RELEASE" "$ASSET"`); err != nil || got != hex64 {
		t.Errorf("digest on base tools = %q (%v), want %q", got, err, hex64)
	}
	if _, err := run(`release_lists_asset "$RELEASE" "$ASSET"`); err != nil {
		t.Errorf("release_lists_asset rejected a listed asset: %v", err)
	}
	if _, err := run(`release_lists_asset "$RELEASE" "absent.tar.gz"`); err == nil {
		t.Error("release_lists_asset accepted an asset the release does not list")
	}
	// The download URL repeats the filename; a listing must come from "name".
	if _, err := run(`release_lists_asset '{"assets":[{"url":"https://x/urnetwork-provider-v1-freebsd-amd64.tar.gz","name":"other.txt"}]}' "$ASSET"`); err == nil {
		t.Error("release_lists_asset matched a browser_download_url instead of a name")
	}
	if got, err := run(`get_asset_digest_from_api_response '{"assets":[{"name":"urnetwork-provider-v1-freebsd-amd64.tar.gz","digest":null}]}' "$ASSET"`); err != nil || got != "" {
		t.Errorf("null digest on base tools = %q (%v), want empty", got, err)
	}
}

// skipWithoutPOSIXShell skips tests that execute the FreeBSD installer with
// /bin/sh. The suite also runs on Windows, where that binary does not exist,
// so without this the test fails on a platform it was never meant to
// exercise. The FreeBSD check runs these against a real kernel and the Linux
// lifecycle check runs them under /bin/sh.
func skipWithoutPOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires /bin/sh; this suite also runs on Windows")
	}
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skipf("no /bin/sh on this host: %v", err)
	}
}

type installerHarnessOpts struct {
	tag         string
	releaseJSON string
	tarballData []byte
	existingBin string
	failRestart bool
	failCpToDst bool
	failMv      bool
	failBackup  bool
	latest      bool
	operation   string
	// noParser runs the installer with jq and python3 hidden from PATH, the
	// shape of a default FreeBSD host.
	noParser bool
}

type installerHarnessResult struct {
	exitCode int
	stdout   string
	stderr   string
	tarCalls []string
	finalBin string
	// versionWritten reports whether the installer left its version marker
	// (.version on Linux, version on FreeBSD) in the install tree, and
	// versionMarker is its content.
	versionWritten bool
	versionMarker  string
}

func runFreeBSDInstaller(t *testing.T, opts installerHarnessOpts) installerHarnessResult {
	t.Helper()
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")

	tempDir := t.TempDir()
	installPath := filepath.Join(tempDir, "install")
	stateDir := filepath.Join(tempDir, "state")
	rcScript := filepath.Join(tempDir, "rc.d", "urnetwork")
	tarballPath := filepath.Join(tempDir, "provider.tar.gz")
	releaseJSONPath := filepath.Join(tempDir, "release.json")
	logDir := filepath.Join(tempDir, "logs")
	_ = os.MkdirAll(logDir, 0o755)

	if err := os.WriteFile(tarballPath, opts.tarballData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releaseJSONPath, []byte(opts.releaseJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if opts.existingBin != "" {
		_ = os.MkdirAll(filepath.Join(installPath, "bin"), 0o755)
		_ = os.WriteFile(filepath.Join(installPath, "bin", "urnetwork"), []byte(opts.existingBin), 0o755)
	}

	fnListsAsset := extractShellFunc(t, script, "release_lists_asset")
	fnScan := extractShellFunc(t, script, "release_asset_scan")
	fnDigest := extractShellFunc(t, script, "get_asset_digest_from_api_response")
	fnCompute := extractShellFunc(t, script, "compute_sha256_file")
	fnVerify := extractShellFunc(t, script, "verify_sha256_file")
	fnDoInstall := extractShellFunc(t, script, "do_install")
	if fnListsAsset == "" {
		t.Fatal("could not extract release_lists_asset from FreeBSD script")
	}
	if fnScan == "" {
		t.Fatal("could not extract release_asset_scan from FreeBSD script")
	}
	if fnDoInstall == "" {
		t.Fatal("could not extract do_install from FreeBSD script")
	}

	tag := opts.tag
	if tag == "" {
		tag = "v2026.10.27"
	}
	if opts.latest {
		tag = "latest"
	}
	failRestartVal := "0"
	if opts.failRestart {
		failRestartVal = "1"
	}
	failCpVal := "0"
	if opts.failCpToDst {
		failCpVal = "1"
	}
	failMvVal := "0"
	if opts.failMv {
		failMvVal = "1"
	}

	harness := fmt.Sprintf(`
pr_err() { printf "[ERROR] %%s\n" "$*" >&2; }
pr_warn() { printf "[WARN] %%s\n" "$*" >&2; }
pr_info() { printf "[INFO] %%s\n" "$*"; }
priv() { "$@"; }
sudo() { "$@"; }
user_shell() { echo "/bin/sh"; }
assert_state_path_safe() { return 0; }
set_rc_conf_default() { return 0; }
write_rc_script_body() { echo "# rc"; }
install_wrapper_tool() { return 0; }
install_cleanup() { return 0; }
resolve_service_user() { echo "testuser"; }
service_running() { return 0; }
restart_service() {
    if [ "${FAIL_RESTART:-0}" = "1" ]; then
        return 1
    fi
    return 0
}
load_service() {
    if [ "${FAIL_RESTART:-0}" = "1" ]; then
        return 1
    fi
    return 0
}
http_fetch() {
    _url="$1"
    _out="${2:-}"
    if [ -n "$_out" ]; then
        cp %q "$_out"
        return 0
    else
        cat %q
        return 0
    fi
}
ln() {
    if [ "${FAIL_BACKUP:-0}" = "1" ]; then
        case "$*" in *"/bin/.urnetwork.bak."*) return 1;; esac
    fi
    command ln "$@"
}
cp() {
    if [ "${FAIL_BACKUP:-0}" = "1" ]; then
        case "$*" in *"/bin/.urnetwork.bak."*|*"/bin/urnetwork.old") return 1;; esac
    fi
    if [ "${FAIL_CP:-0}" = "1" ]; then
        case "$*" in
            *"/bin/.urnetwork"*|*"/bin/urnetwork"*)
                echo "simulated cp failure" >&2
                return 1
                ;;
        esac
    fi
    command cp "$@"
}
mv() {
    if [ "${FAIL_MV:-0}" = "1" ]; then
        case "$*" in
            *"-f "*"$install_path/bin/urnetwork")
                echo "simulated mv failure" >&2
                return 1
                ;;
        esac
    fi
    command mv "$@"
}
tar() {
    echo "tar $*" >> %q
    if [ -d "$tmpdir" ]; then
        mkdir -p "$tmpdir/freebsd/amd64"
        echo "extracted-new-binary" > "$tmpdir/freebsd/amd64/provider"
        echo "extracted-new-binary" > "$tmpdir/provider"
        chmod 755 "$tmpdir/provider" "$tmpdir/freebsd/amd64/provider"
    fi
    return 0
}
`, tarballPath, releaseJSONPath, filepath.Join(logDir, "tar.log"))

	driver := fmt.Sprintf(`
service_user="testuser"
version=%q
install_path=%q
provider_bin="$install_path/bin/urnetwork"
state_dir=%q
rc_script=%q
me="urnet-tools"
github_api="http://mock"
FAIL_RESTART=%q
FAIL_CP=%q
FAIL_MV=%q
do_install "$version"
`, tag, installPath, stateDir, rcScript, failRestartVal, failCpVal, failMvVal)

	fullScript := harness + "\n" + fnScan + "\n" + fnListsAsset + "\n" + fnDigest + "\n" + fnCompute + "\n" + fnVerify + "\n" + fnDoInstall + "\n" + driver
	cmd := exec.Command("/bin/sh", "-c", fullScript)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	if opts.noParser {
		cmd.Env = []string{"PATH=" + pathWithoutParsers(t)}
	}
	if opts.failBackup {
		cmd.Env = append(cmd.Env, "FAIL_BACKUP=1")
	}
	out, err := cmd.CombinedOutput()

	var exitCode int
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	var tarCalls []string
	if tarLogBytes, rerr := os.ReadFile(filepath.Join(logDir, "tar.log")); rerr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(tarLogBytes)), "\n") {
			if strings.TrimSpace(line) != "" {
				tarCalls = append(tarCalls, line)
			}
		}
	}

	finalBinBytes, _ := os.ReadFile(filepath.Join(installPath, "bin", "urnetwork"))
	versionBytes, verr := os.ReadFile(filepath.Join(installPath, "version"))

	return installerHarnessResult{
		exitCode:       exitCode,
		stdout:         string(out),
		stderr:         string(out),
		tarCalls:       tarCalls,
		finalBin:       string(finalBinBytes),
		versionWritten: verr == nil,
		versionMarker:  string(versionBytes),
	}
}

func runLinuxInstaller(t *testing.T, opts installerHarnessOpts) installerHarnessResult {
	t.Helper()
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_Linux.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")

	tempDir := t.TempDir()
	installPath := filepath.Join(tempDir, "install")
	tarballPath := filepath.Join(tempDir, "provider.tar.gz")
	releaseJSONPath := filepath.Join(tempDir, "release.json")
	logDir := filepath.Join(tempDir, "logs")
	_ = os.MkdirAll(logDir, 0o755)

	if err := os.WriteFile(tarballPath, opts.tarballData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releaseJSONPath, []byte(opts.releaseJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if opts.existingBin != "" {
		_ = os.MkdirAll(filepath.Join(installPath, "bin"), 0o755)
		_ = os.WriteFile(filepath.Join(installPath, "bin", "urnetwork"), []byte(opts.existingBin), 0o755)
	}

	fnGetVersion := extractShellFunc(t, script, "get_version_from_api_response")
	fnGetDate := extractShellFunc(t, script, "get_release_date_from_api_response")
	fnListsAsset := extractShellFunc(t, script, "release_lists_asset")
	if fnListsAsset == "" {
		t.Fatal("could not extract release_lists_asset from Linux script")
	}
	fnDigest := extractShellFunc(t, script, "get_asset_digest_from_api_response")
	fnCompute := extractShellFunc(t, script, "compute_sha256_file")
	fnVerify := extractShellFunc(t, script, "verify_sha256_file")
	fnSanitize := extractShellFunc(t, script, "sanitize_restart_dropins")
	fnInstallSystemd := extractShellFunc(t, script, "install_systemd_units")
	fnDoInstall := extractShellFunc(t, script, "do_install")
	if fnDoInstall == "" {
		t.Fatal("could not extract do_install from Linux script")
	}

	tag := opts.tag
	if tag == "" {
		tag = "v2026.10.27"
	}
	op := opts.operation
	if op == "" {
		op = "install"
	}
	failRestartVal := "0"
	if opts.failRestart {
		failRestartVal = "1"
	}
	failCpVal := "0"
	if opts.failCpToDst {
		failCpVal = "1"
	}
	failMvVal := "0"
	if opts.failMv {
		failMvVal = "1"
	}

	harness := fmt.Sprintf(`
pr_err() { printf "[ERROR] %%s\n" "$*" >&2; }
pr_warn() { printf "[WARN] %%s\n" "$*" >&2; }
pr_info() { printf "[INFO] %%s\n" "$*"; }
sudo() { "$@"; }
install_downloader() { return 0; }
install_json_parser() { return 0; }
openrc_maybe_redirect_install_path() { return 0; }
stop_systemd_units() { return 0; }
stop_openrc_units() { return 0; }
ensure_tools_on_path() { return 0; }
add_path_blocks() { return 0; }
func_root_guard() { return 0; }
network_fetch() {
    cat %q
}
download_asset() {
    _src="$1"
    _dst="$2"
    cp %q "$_dst"
}
ln() {
    if [ "${FAIL_BACKUP:-0}" = "1" ]; then
        case "$*" in *"/bin/.urnetwork.bak."*) return 1;; esac
    fi
    command ln "$@"
}
cp() {
    if [ "${FAIL_BACKUP:-0}" = "1" ]; then
        case "$*" in *"/bin/.urnetwork.bak."*|*"/bin/urnetwork.old") return 1;; esac
    fi
    if [ "${FAIL_CP:-0}" = "1" ]; then
        case "$*" in
            *"/bin/.urnetwork"*|*"/bin/urnetwork"*)
                echo "simulated cp failure" >&2
                return 1
                ;;
        esac
    fi
    command cp "$@"
}
mv() {
    if [ "${FAIL_MV:-0}" = "1" ]; then
        case "$*" in
            *"-f "*"$install_path/bin/urnetwork")
                echo "simulated mv failure" >&2
                return 1
                ;;
        esac
    fi
    command mv "$@"
}
tar() {
    echo "tar $*" >> %q
    if [ -d "$workdir" ]; then
        mkdir -p "$workdir/linux/amd64"
        echo "extracted-new-binary" > "$workdir/linux/amd64/provider"
        chmod 755 "$workdir/linux/amd64/provider"
    fi
    return 0
}
systemctl() {
    if [ "$1" = "--user" ] && [ "$2" = "start" ]; then
        if [ "${FAIL_RESTART:-0}" = "1" ]; then
            return 1
        fi
    fi
    return 0
}
`, releaseJSONPath, tarballPath, filepath.Join(logDir, "tar.log"))

	driver := fmt.Sprintf(`
tag=%q
operation=%q
install_path=%q
script_rundir=%q
api_base="http://mock"
urnet_github_dl_base="http://mock"
urnet_dl_base="http://mock"
api_url="http://mock/releases/tags/$tag"
arch="amd64"
has_systemd=1
has_openrc=0
FAIL_RESTART=%q
FAIL_CP=%q
FAIL_MV=%q
systemd_service="%s/urnetwork.service"
systemd_update_service="%s/urnetwork-update.service"
systemd_update_timer="%s/urnetwork-update.timer"
systemd_userdir="%s"
systemd_units_stopped=1
do_install
`, tag, op, installPath, tempDir, failRestartVal, failCpVal, failMvVal, tempDir, tempDir, tempDir, tempDir)

	fullScript := harness + "\n" + fnGetVersion + "\n" + fnGetDate + "\n" + fnListsAsset + "\n" + fnDigest + "\n" + fnCompute + "\n" + fnVerify + "\n" + fnSanitize + "\n" + fnInstallSystemd + "\n" + fnDoInstall + "\n" + driver
	cmd := exec.Command("/bin/sh", "-c", fullScript)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	if opts.failBackup {
		cmd.Env = append(cmd.Env, "FAIL_BACKUP=1")
	}
	out, err := cmd.CombinedOutput()

	var exitCode int
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	var tarCalls []string
	if tarLogBytes, rerr := os.ReadFile(filepath.Join(logDir, "tar.log")); rerr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(tarLogBytes)), "\n") {
			if strings.TrimSpace(line) != "" {
				tarCalls = append(tarCalls, line)
			}
		}
	}

	finalBinBytes, _ := os.ReadFile(filepath.Join(installPath, "bin", "urnetwork"))
	versionBytes, verr := os.ReadFile(filepath.Join(installPath, ".version"))

	return installerHarnessResult{
		exitCode:       exitCode,
		stdout:         string(out),
		stderr:         string(out),
		tarCalls:       tarCalls,
		finalBin:       string(finalBinBytes),
		versionWritten: verr == nil,
		versionMarker:  string(versionBytes),
	}
}

// 1. A provider tarball whose digest does not match: the install aborts before
// extraction and the error names the asset and digests.
func TestInstallerProviderTarballDigestMismatchAborts(t *testing.T) {
	tarballContent := []byte("actual-tarball-payload-bytes")
	sum := sha256.Sum256(tarballContent)
	actualDigest := hex.EncodeToString(sum[:])
	expectedDigest := strings.Repeat("d", 64)

	tag := "v2026.10.27"

	for _, tc := range []struct {
		name      string
		assetName string
		run       func(*testing.T, installerHarnessOpts) installerHarnessResult
	}{
		{"FreeBSD", "urnetwork-provider-" + tag + "-freebsd-amd64.tar.gz", runFreeBSDInstaller},
		{"Linux", "urnetwork-provider-" + tag + ".tar.gz", runLinuxInstaller},
	} {
		t.Run(tc.name, func(t *testing.T) {
			releaseJSON := fmt.Sprintf(`{
		"tag_name": %q,
		"published_at": "2026-10-27T00:00:00Z",
		"assets": [
			{"name": %q, "digest": "sha256:%s"}
		]
	}`, tag, tc.assetName, expectedDigest)
			res := tc.run(t, installerHarnessOpts{
				tag:         tag,
				releaseJSON: releaseJSON,
				tarballData: tarballContent,
			})
			if res.exitCode == 0 {
				t.Fatalf("expected install to abort on digest mismatch, got exit 0 (out:\n%s)", res.stdout)
			}
			if len(res.tarCalls) != 0 {
				t.Fatalf("tar was invoked (%v); must abort before extraction", res.tarCalls)
			}
			combinedOut := res.stdout + "\n" + res.stderr
			if !strings.Contains(combinedOut, tc.assetName) {
				t.Errorf("error output missing asset name %q:\n%s", tc.assetName, combinedOut)
			}
			if !strings.Contains(combinedOut, expectedDigest) {
				t.Errorf("error output missing expected digest %q:\n%s", expectedDigest, combinedOut)
			}
			if !strings.Contains(combinedOut, actualDigest) {
				t.Errorf("error output missing actual digest %q:\n%s", actualDigest, combinedOut)
			}
		})
	}
}

// 2. A matching digest: extraction proceeds.
func TestInstallerProviderTarballDigestMatchProceeds(t *testing.T) {
	tarballContent := []byte("valid-matching-tarball-bytes")
	sum := sha256.Sum256(tarballContent)
	actualDigest := hex.EncodeToString(sum[:])

	tag := "v2026.10.27"

	for _, tc := range []struct {
		name      string
		assetName string
		run       func(*testing.T, installerHarnessOpts) installerHarnessResult
	}{
		{"FreeBSD", "urnetwork-provider-" + tag + "-freebsd-amd64.tar.gz", runFreeBSDInstaller},
		{"Linux", "urnetwork-provider-" + tag + ".tar.gz", runLinuxInstaller},
	} {
		t.Run(tc.name, func(t *testing.T) {
			releaseJSON := fmt.Sprintf(`{
		"tag_name": %q,
		"published_at": "2026-10-27T00:00:00Z",
		"assets": [
			{"name": %q, "digest": "sha256:%s"}
		]
	}`, tag, tc.assetName, actualDigest)
			res := tc.run(t, installerHarnessOpts{
				tag:         tag,
				releaseJSON: releaseJSON,
				tarballData: tarballContent,
			})
			if res.exitCode != 0 {
				t.Fatalf("expected matching digest install to succeed, got exit %d (out:\n%s)", res.exitCode, res.stdout)
			}
			if len(res.tarCalls) == 0 {
				t.Fatal("expected tar extraction to proceed, but tar was never invoked")
			}
			if strings.Contains(res.stdout+res.stderr, "mismatch") {
				t.Errorf("unexpected mismatch message in output:\n%s", res.stdout)
			}
		})
	}
}

// 3. A release with no digest: one warning, install proceeds.
func TestInstallerProviderTarballNoDigestWarnsAndProceeds(t *testing.T) {
	tarballContent := []byte("tarball-without-release-digest")
	tag := "v2026.10.27"

	for _, tc := range []struct {
		name      string
		assetName string
		run       func(*testing.T, installerHarnessOpts) installerHarnessResult
	}{
		{"FreeBSD", "urnetwork-provider-" + tag + "-freebsd-amd64.tar.gz", runFreeBSDInstaller},
		{"Linux", "urnetwork-provider-" + tag + ".tar.gz", runLinuxInstaller},
	} {
		t.Run(tc.name, func(t *testing.T) {
			releaseJSON := fmt.Sprintf(`{
		"tag_name": %q,
		"published_at": "2026-10-27T00:00:00Z",
		"assets": [
			{"name": %q}
		]
	}`, tag, tc.assetName)
			res := tc.run(t, installerHarnessOpts{
				tag:         tag,
				releaseJSON: releaseJSON,
				tarballData: tarballContent,
			})
			if res.exitCode != 0 {
				t.Fatalf("expected missing digest install to proceed, got exit %d (out:\n%s)", res.exitCode, res.stdout)
			}
			if len(res.tarCalls) == 0 {
				t.Fatal("expected tar extraction to proceed for undigested release")
			}
			combinedOut := res.stdout + "\n" + res.stderr
			if strings.Contains(combinedOut, "mismatch") {
				t.Errorf("missing digest must not be treated as a mismatch:\n%s", combinedOut)
			}
			// stdout and stderr are the same combined stream in the harness,
			// so count the warnings in one of them only.
			providerWarns := 0
			for _, line := range strings.Split(res.stdout, "\n") {
				if strings.Contains(line, "[WARN]") && strings.Contains(line, tc.assetName) {
					providerWarns++
				}
			}
			if providerWarns != 1 {
				t.Errorf("expected exactly one warning naming unverified asset %q, got %d:\n%s", tc.assetName, providerWarns, res.stdout)
			}
		})
	}
}

// 4. An interrupted replacement (stub mv/cp to fail at the right moment)
// leaves the configured ExecPath present or restores the backup — never absent.
func TestInstallerInterruptedReplacementLeavesBinaryIntact(t *testing.T) {
	existingPayload := "pre-existing-working-binary"
	tarballContent := []byte("new-tarball-payload")
	tag := "v2026.10.27"
	sum := sha256.Sum256(tarballContent)
	actualDigest := hex.EncodeToString(sum[:])

	for _, tc := range []struct {
		name      string
		assetName string
		run       func(*testing.T, installerHarnessOpts) installerHarnessResult
	}{
		{"FreeBSD", "urnetwork-provider-" + tag + "-freebsd-amd64.tar.gz", runFreeBSDInstaller},
		{"Linux", "urnetwork-provider-" + tag + ".tar.gz", runLinuxInstaller},
	} {
		releaseJSON := fmt.Sprintf(`{
		"tag_name": %q,
		"published_at": "2026-10-27T00:00:00Z",
		"assets": [
			{"name": %q, "digest": "sha256:%s"}
		]
	}`, tag, tc.assetName, actualDigest)
		t.Run(tc.name+"/cp_failure", func(t *testing.T) {
			res := tc.run(t, installerHarnessOpts{
				tag:         tag,
				operation:   "update",
				releaseJSON: releaseJSON,
				tarballData: tarballContent,
				existingBin: existingPayload,
				failCpToDst: true,
			})
			if res.finalBin == "" {
				t.Fatalf("interrupted replacement left executable absent! out:\n%s", res.stdout)
			}
			if res.finalBin != existingPayload {
				t.Errorf("interrupted replacement changed binary, want %q, got %q", existingPayload, res.finalBin)
			}
		})
		t.Run(tc.name+"/mv_failure", func(t *testing.T) {
			res := tc.run(t, installerHarnessOpts{
				tag:         tag,
				operation:   "update",
				releaseJSON: releaseJSON,
				tarballData: tarballContent,
				existingBin: existingPayload,
				failMv:      true,
			})
			if res.finalBin == "" {
				t.Fatalf("interrupted replacement left executable absent! out:\n%s", res.stdout)
			}
			if res.finalBin != existingPayload {
				t.Errorf("interrupted replacement changed binary, want %q, got %q", existingPayload, res.finalBin)
			}
		})
	}
}

func TestInstallerBackupFailurePreservesLiveBinary(t *testing.T) {
	const tag = "v2026.10.27"
	const old = "pre-existing-working-binary"
	for _, tc := range []struct {
		name  string
		asset string
		run   func(*testing.T, installerHarnessOpts) installerHarnessResult
	}{
		{"FreeBSD", "urnetwork-provider-" + tag + "-freebsd-amd64.tar.gz", runFreeBSDInstaller},
		{"Linux", "urnetwork-provider-" + tag + ".tar.gz", runLinuxInstaller},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := fmt.Sprintf(`{"tag_name":%q,"assets":[{"name":%q}]}`, tag, tc.asset)
			res := tc.run(t, installerHarnessOpts{tag: tag, operation: "update", releaseJSON: release, tarballData: []byte("payload"), existingBin: old, failBackup: true})
			if res.exitCode == 0 || res.finalBin != old || !strings.Contains(res.stdout, "Could not back up") {
				t.Fatalf("backup failure did not abort before replacing live binary: exit=%d binary=%q output=%s", res.exitCode, res.finalBin, res.stdout)
			}
		})
	}
}

// 5. A failed restart after install restores the previous binary.
func TestInstallerFailedRestartRestoresBackup(t *testing.T) {
	existingPayload := "pre-existing-working-binary"
	tarballContent := []byte("new-tarball-payload")
	tag := "v2026.10.27"
	sum := sha256.Sum256(tarballContent)
	actualDigest := hex.EncodeToString(sum[:])

	for _, tc := range []struct {
		name      string
		assetName string
		run       func(*testing.T, installerHarnessOpts) installerHarnessResult
	}{
		{"FreeBSD", "urnetwork-provider-" + tag + "-freebsd-amd64.tar.gz", runFreeBSDInstaller},
		{"Linux", "urnetwork-provider-" + tag + ".tar.gz", runLinuxInstaller},
	} {
		releaseJSON := fmt.Sprintf(`{
		"tag_name": %q,
		"published_at": "2026-10-27T00:00:00Z",
		"assets": [
			{"name": %q, "digest": "sha256:%s"}
		]
	}`, tag, tc.assetName, actualDigest)
		t.Run(tc.name, func(t *testing.T) {
			res := tc.run(t, installerHarnessOpts{
				tag:         tag,
				operation:   "update",
				releaseJSON: releaseJSON,
				tarballData: tarballContent,
				existingBin: existingPayload,
				failRestart: true,
			})
			if res.exitCode == 0 {
				t.Fatalf("expected install to fail when restart fails, got exit 0 (out:\n%s)", res.stdout)
			}
			if res.finalBin != existingPayload {
				t.Errorf("failed restart did not restore previous binary: got %q, want %q (out:\n%s)", res.finalBin, existingPayload, res.stdout)
			}
			combined := res.stdout + "\n" + res.stderr
			if !strings.Contains(combined, "Restored") && !strings.Contains(combined, "restored") {
				t.Errorf("expected message indicating rollback/restoration of previous binary:\n%s", combined)
			}
		})
	}
}

// assertInstallRefused checks the fail-closed contract shared by both
// installers: a non-zero exit, no tarball extraction, and an error that names
// the tag and the provider asset it could not verify.
func assertInstallRefused(t *testing.T, res installerHarnessResult, tag, assetName string) {
	t.Helper()
	combined := res.stdout + "\n" + res.stderr
	if res.exitCode == 0 {
		t.Fatalf("expected refusal (non-zero exit), got exit 0:\n%s", combined)
	}
	if len(res.tarCalls) != 0 {
		t.Fatalf("tar ran (%v) before the release metadata was verified", res.tarCalls)
	}
	if !strings.Contains(strings.ToLower(combined), "refusing") {
		t.Errorf("refusal message does not say it refused:\n%s", combined)
	}
	for _, want := range []string{tag, assetName} {
		if !strings.Contains(combined, want) {
			t.Errorf("refusal output missing %q:\n%s", want, combined)
		}
	}
}

// 6. Release metadata unavailable (API unreachable or rate-limited, empty body,
// no tag fallback): the installer refuses before download and extraction. An
// unreadable release used to leave the digest empty and install unverified.
func TestInstallerRefusesWithoutReleaseMetadata(t *testing.T) {
	tag := "v2026.10.27"

	for _, tc := range []struct {
		name      string
		assetName string
		run       func(*testing.T, installerHarnessOpts) installerHarnessResult
	}{
		{"FreeBSD", "urnetwork-provider-" + tag + "-freebsd-amd64.tar.gz", runFreeBSDInstaller},
		{"Linux", "urnetwork-provider-" + tag + ".tar.gz", runLinuxInstaller},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.run(t, installerHarnessOpts{
				tag:         tag,
				releaseJSON: "",
				tarballData: []byte("tarball-with-no-verifiable-release"),
			})
			assertInstallRefused(t, res, tag, tc.assetName)
		})
	}
}

// 7. Release metadata readable but the named provider asset absent (an error
// body or a release without the tarball): refuse, naming the asset.
func TestInstallerRefusesWhenProviderAssetAbsent(t *testing.T) {
	tag := "v2026.10.27"

	for _, tc := range []struct {
		name      string
		assetName string
		run       func(*testing.T, installerHarnessOpts) installerHarnessResult
	}{
		{"FreeBSD", "urnetwork-provider-" + tag + "-freebsd-amd64.tar.gz", runFreeBSDInstaller},
		{"Linux", "urnetwork-provider-" + tag + ".tar.gz", runLinuxInstaller},
	} {
		t.Run(tc.name, func(t *testing.T) {
			releaseJSON := fmt.Sprintf(`{
		"tag_name": %q,
		"published_at": "2026-10-27T00:00:00Z",
		"assets": [
			{"name": "urnet-tools-unrelated.tar.gz", "digest": "sha256:%s"}
		]
	}`, tag, strings.Repeat("e", 64))
			res := tc.run(t, installerHarnessOpts{
				tag:         tag,
				releaseJSON: releaseJSON,
				tarballData: []byte("tarball-absent-from-release"),
			})
			assertInstallRefused(t, res, tag, tc.assetName)
		})
	}
}

// 8. The version marker is written only once the units are installed and the
// service has started. A failed start leaves no marker, so a rollback never
// leaves the file naming the release that did not run.
func TestInstallerVersionMarkerOnlyAfterServiceStarts(t *testing.T) {
	tarballContent := []byte("marker-ordering-tarball")
	tag := "v2026.10.27"
	sum := sha256.Sum256(tarballContent)
	actualDigest := hex.EncodeToString(sum[:])

	for _, tc := range []struct {
		name      string
		assetName string
		run       func(*testing.T, installerHarnessOpts) installerHarnessResult
	}{
		{"FreeBSD", "urnetwork-provider-" + tag + "-freebsd-amd64.tar.gz", runFreeBSDInstaller},
		{"Linux", "urnetwork-provider-" + tag + ".tar.gz", runLinuxInstaller},
	} {
		releaseJSON := fmt.Sprintf(`{
		"tag_name": %q,
		"published_at": "2026-10-27T00:00:00Z",
		"assets": [
			{"name": %q, "digest": "sha256:%s"}
		]
	}`, tag, tc.assetName, actualDigest)

		t.Run(tc.name+"/started", func(t *testing.T) {
			res := tc.run(t, installerHarnessOpts{
				tag:         tag,
				releaseJSON: releaseJSON,
				tarballData: tarballContent,
			})
			if res.exitCode != 0 {
				t.Fatalf("expected install to succeed, got exit %d (out:\n%s)", res.exitCode, res.stdout)
			}
			if !res.versionWritten {
				t.Fatalf("version marker not written after a successful start (out:\n%s)", res.stdout)
			}
			if !strings.Contains(res.versionMarker, "2026.10.27") {
				t.Errorf("version marker %q does not name the installed release", res.versionMarker)
			}
		})
		t.Run(tc.name+"/start_fails", func(t *testing.T) {
			res := tc.run(t, installerHarnessOpts{
				tag:         tag,
				releaseJSON: releaseJSON,
				tarballData: tarballContent,
				failRestart: true,
			})
			if res.exitCode == 0 {
				t.Fatalf("expected install to fail when the service does not start, got exit 0 (out:\n%s)", res.stdout)
			}
			if res.versionWritten {
				t.Errorf("version marker written (%q) for a release whose service did not start", res.versionMarker)
			}
		})
	}
}

// pathWithoutParsers returns a PATH directory holding every tool from
// /usr/bin and /bin except jq and python, so `command -v jq` fails the way it
// does on a default FreeBSD host while the base utilities still resolve.
func pathWithoutParsers(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, src := range []string{"/bin", "/usr/bin"} {
		entries, err := os.ReadDir(src)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if name == "jq" || strings.HasPrefix(name, "python") {
				continue
			}
			_ = os.Symlink(filepath.Join(src, name), filepath.Join(dir, name))
		}
	}
	return dir
}

// Without a JSON parser, refuse before extraction or installation even when
// release metadata contains a valid digest.
func TestFreeBSDInstallerWithoutJSONParser(t *testing.T) {
	tag := "v2026.10.27"
	asset := "urnetwork-provider-" + tag + "-freebsd-amd64.tar.gz"
	tarballContent := []byte("new-tarball-payload")
	sum := sha256.Sum256(tarballContent)
	releaseFor := func(digest string) string {
		return fmt.Sprintf(`{
  "url": "https://api.github.com/repos/x/y/releases/1",
  "tag_name": %q,
  "name": %q,
  "assets": [
    {
      "name": "urnet-tools-freebsd-amd64",
      "digest": "sha256:%s"
    },
    {
      "url": "https://api.github.com/repos/x/y/releases/assets/2",
      "name": %q,
      "uploader": {"login": "bot"},
      "size": 19,
      "digest": "sha256:%s"
    }
  ]
}`, tag, tag, strings.Repeat("a", 64), asset, digest)
	}

	t.Run("digest_match_aborts", func(t *testing.T) {
		res := runFreeBSDInstaller(t, installerHarnessOpts{
			tag:         tag,
			releaseJSON: releaseFor(hex.EncodeToString(sum[:])),
			tarballData: tarballContent,
			noParser:    true,
			latest:      true,
		})
		if res.exitCode == 0 || len(res.tarCalls) != 0 || res.finalBin != "" {
			t.Fatalf("install must refuse without a JSON parser (exit=%d tar=%v bin=%q):\n%s", res.exitCode, res.tarCalls, res.finalBin, res.stdout)
		}
		if !strings.Contains(res.stderr, "Neither python3 nor jq is available") || !strings.Contains(res.stderr, "pkg install jq") {
			t.Fatalf("missing parser error or installation hint: %s", res.stderr)
		}
	})

	t.Run("digest_mismatch_aborts", func(t *testing.T) {
		res := runFreeBSDInstaller(t, installerHarnessOpts{
			tag:         tag,
			releaseJSON: releaseFor(strings.Repeat("b", 64)),
			tarballData: tarballContent,
			noParser:    true,
			latest:      true,
		})
		if res.exitCode == 0 {
			t.Fatalf("mismatched digest installed without a parser:\n%s", res.stdout)
		}
		if len(res.tarCalls) != 0 {
			t.Fatalf("tar ran despite a digest mismatch: %v", res.tarCalls)
		}
	})
}
