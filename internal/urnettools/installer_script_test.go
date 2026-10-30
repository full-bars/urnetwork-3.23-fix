package urnettools

import (
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
		case (strings.Contains(line, "http_fetch ") || strings.Contains(line, "curl -fsSL")) && firstFetch < 0:
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
	// The chown runs on '.' after a `cd "$state_dir"` in the SAME subshell. A cd
	// in its own `if ( cd ... )` subshell leaves the installer's working
	// directory unchanged, so the chown would hit wherever the operator ran the
	// script from. The entered directory must match the service home by
	// pwd -P, and the chown must not recurse: -h guards symlinks, not hard
	// links the service user can plant to root-owned files.
	stateOwned := strings.Contains(body, `cd "$state_dir"`) &&
		strings.Contains(body, `pwd -P`) &&
		strings.Contains(body, `chown -h "$service_user" .`)
	if !stateOwned {
		t.Error(`do_install does not hand the state directory to the service user (want cd "$state_dir", a pwd -P check and chown -h "$service_user" .)`)
	}
	if strings.Contains(body, `if ( cd "$state_dir"`) {
		t.Error("the state dir cd is in its own subshell, so the chown runs in the installer's working directory")
	}
	if strings.Contains(body, `chown -R -h "$service_user"`) {
		t.Error("do_install recurses a root chown through the service user's tree")
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
func extractShellFunc(t *testing.T, script, name string) string {
	t.Helper()
	start := strings.Index(script, name+"() {")
	if start < 0 {
		return ""
	}
	rest := script[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		return ""
	}
	return rest[:end+2]
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
	i := strings.Index(script, `tool_digest="$(printf`)
	j := strings.Index(script, `if [ -z "$tool_digest" ]`)
	if i < 0 || j < 0 || j <= i {
		t.Fatal("could not locate the tool digest lookup in the FreeBSD installer")
	}
	block := script[i:j]

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
	jqStart := strings.Index(block, "jq -r --arg a \"$tool_asset\" '")
	if !haveJq {
		t.Log("jq not installed here; exercising only the python path")
	} else if jqStart < 0 {
		t.Error("could not find the jq digest expression in the installer; the marker moved and this test would otherwise pass vacuously")
	} else {
		expr := block[jqStart+len("jq -r --arg a \"$tool_asset\" '"):]
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
		pyEnd := strings.Index(py, "' \"$tool_asset\"")
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

// The installer puts the provider in a ROOT-owned tree outside the service
// user's home, so the account that runs the binary cannot rewrite it. Both
// sides carry that path as a literal: the shell script, and the FreeBSD
// discovery fallback that has to find a stopped install. This fails when one
// moves without the other, which is what the script's "keep the two in step"
// comment promises.
func TestFreeBSDRootOwnedInstallPathMatchesDiscovery(t *testing.T) {
	const installPath = "/usr/local/libexec/urnetwork-provider"
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")
	if !strings.Contains(script, `install_path="`+installPath+`"`) {
		t.Errorf("installer does not install into the root-owned tree %s", installPath)
	}
	// The old per-user tree must not be where a NEW install writes: it left
	// the service user owning a binary root executes.
	if strings.Contains(script, `install_path="$service_home/`) {
		t.Error("installer still installs under the service user's home")
	}
	d, err := os.ReadFile("discover_freebsd.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(d), `"`+installPath+`/bin/urnetwork"`) {
		t.Errorf("discovery fallback does not look in %s/bin/urnetwork; a stopped install would be undiscoverable", installPath)
	}
}
