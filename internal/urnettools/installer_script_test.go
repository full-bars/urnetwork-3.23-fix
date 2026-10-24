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
	var execLines []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		execLines = append(execLines, trimmed)
	}

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
	// is its state directory, where its config and JWT live.
	rootOwned := strings.Contains(body, `chown -R root:wheel "$install_path"`) ||
		strings.Contains(body, `chown -R root "$install_path"`)
	if !rootOwned {
		t.Error("do_install does not keep the install tree root-owned")
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
	j := strings.Index(script, `if [ -n "$tool_digest" ]`)
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
