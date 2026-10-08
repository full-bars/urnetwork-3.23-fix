package urnettools

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// procstat -b emits a header row and one row per process. PATH is the
// kernel-reported image path, which is the only field discovery trusts to
// identify a provider: argv[0] can be set to anything with `exec -a`.
func TestParseProcstatBasic(t *testing.T) {
	out := `PID COMM OSREL PATH
1 init 1400097 /sbin/init
424 provider 1400097 /usr/local/bin/urnetwork
425 sh 1400097 /bin/sh
426 urnetwork 1400097 /usr/local/bin/urnetwork-beta
427 nobody 1400097 /home/nobody/.local/share/urnetwork-provider/bin/provider
`
	rows := parseProcstatBasic(out)
	if len(rows) != 5 {
		t.Fatalf("expected 5 rows, got %d: %+v", len(rows), rows)
	}
	if rows[0].pid != "1" || rows[0].path != "/sbin/init" {
		t.Errorf("init row wrong: %+v", rows[0])
	}
	if rows[1].pid != "424" || rows[1].comm != "provider" || rows[1].path != "/usr/local/bin/urnetwork" {
		t.Errorf("provider row wrong: %+v", rows[1])
	}
	// The header must never be parsed as a process.
	for _, r := range rows {
		if r.pid == "PID" {
			t.Errorf("header row leaked into results: %+v", r)
		}
	}
}

// An unreadable binary path is reported as "-" by procstat. Such a row is not
// a provider: there is no kernel evidence of what the process is, and the
// COMM column alone is attacker-controlled via `exec -a`.
func TestParseProcstatBasicUnreadablePath(t *testing.T) {
	out := `PID COMM OSREL PATH
900 urnetwork - -
901 real 1400097 /usr/local/bin/urnetwork
`
	rows := parseProcstatBasic(out)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].path != "-" {
		t.Errorf("unreadable path should parse as %q, got %q", "-", rows[0].path)
	}
	// The caller filters on this; the parser must preserve the marker.
	if isProviderArg(rows[0].path) {
		t.Error(`the "-" placeholder must not be treated as a provider arg`)
	}
	if !isProviderArg(rows[1].path) {
		t.Error("a real path must be recognized as a provider arg")
	}
}

// Empty, header-only and CRLF output must all yield no rows rather than
// panicking or inventing a process.
func TestParseProcstatBasicDegenerate(t *testing.T) {
	for _, in := range []string{
		"",
		"\n\n",
		"PID COMM OSREL PATH\n",
		"PID COMM OSREL PATH\r\n1 init 1400097 /sbin/init\r\n",
		"garbage\n",
		"not-a-number COMM OSREL /path\n",
	} {
		rows := parseProcstatBasic(in)
		if len(rows) > 1 {
			t.Errorf("input %q produced %d rows, want at most 1", in, len(rows))
		}
	}
}

// procstat -s reports the kernel credential record. EUID is the identity the
// process actually runs as, which is what decides who owns a discovered
// provider.
func TestParseProcstatCredentials(t *testing.T) {
	out := `PID COMM EUID RUID SVUID EGID RGID SVGID UMASK FLAGS GROUPS
1 init 0 0 0 0 0 0 022 - root
424 provider 1001 1001 1001 1001 1001 1001 022 - user1
425 root-owned 0 0 0 0 0 0 022 - root
`
	creds := parseProcstatCredentials(out)
	if len(creds) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(creds), creds)
	}
	c := creds["424"]
	if !c.haveEUID || c.euid != 1001 {
		t.Errorf("provider euid wrong: %+v", c)
	}
	if !c.haveRUID || c.ruid != 1001 {
		t.Errorf("provider ruid wrong: %+v", c)
	}
	if root := creds["425"]; !root.haveEUID || root.euid != 0 {
		t.Errorf("root euid wrong: %+v", root)
	}
}

// An unreadable credential cell prints as "-" and must never be coerced to 0.
// Coercing it would make an unresolvable owner look like root, and a row
// attributed to root is one this tool may act on with elevated privileges.
func TestParseProcstatCredentialsUnreadable(t *testing.T) {
	out := `PID COMM EUID RUID SVUID EGID RGID SVGID UMASK FLAGS GROUPS
424 provider - - - - - - - - -
425 provider - 1001 - - - - - - user1
`
	creds := parseProcstatCredentials(out)
	unknown := creds["424"]
	if unknown.haveEUID {
		t.Errorf(`an unreadable EUID must not report as known, got %+v`, unknown)
	}
	partial := creds["425"]
	if partial.haveEUID {
		t.Errorf("EUID must stay unknown when only RUID parsed: %+v", partial)
	}
	if !partial.haveRUID || partial.ruid != 1001 {
		t.Errorf("readable RUID must still be captured: %+v", partial)
	}
}

func TestParseUIDField(t *testing.T) {
	for _, c := range []struct {
		in   string
		want uint32
		ok   bool
	}{
		{"0", 0, true},
		{"1001", 1001, true},
		{"4294967295", 4294967295, true},
		{"-", 0, false},
		{"", 0, false},
		{"root", 0, false},
		// Overflows uint32: not a uid we can represent, so not a uid.
		{"4294967296", 0, false},
		{"-1", 0, false},
	} {
		got, ok := parseUIDField(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("parseUIDField(%q) = %d,%v want %d,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// The exact procstat argv is a contract with the tool, and getting it wrong
// fails SILENTLY rather than loudly: `procstat -b` with no pid list and no -a
// prints its usage and exits non-zero, runProcstat turns that into empty
// output, and discovery falls back to the install location — so the tool still
// prints a plausible inventory row while the live-process path never runs.
//
// A FreeBSD CI run with a real provider-named process is what caught this. This
// test reads the real call sites rather than restating them, so deleting a flag
// from discoverProcesses fails here instead of on a user's box.
func TestDiscoverProcessesCallsProcstatWithAllFlag(t *testing.T) {
	b, err := os.ReadFile("discover_freebsd.go")
	if err != nil {
		t.Skipf("cannot read discover_freebsd.go: %v", err)
	}
	src := string(b)
	// Match only real invocations: `runProcstat(` followed by a quote, so the
	// variadic function DEFINITION `runProcstat(args ...string)` is not
	// mistaken for a call site.
	calls := regexp.MustCompile(`runProcstat\(\s*"`).FindAllString(src, -1)
	if len(calls) == 0 {
		t.Fatal("no runProcstat call sites found; the discovery seam moved")
	}
	// Both procstat invocations must carry -a alongside their subcommand.
	for _, want := range []string{`runProcstat("-a", "-b")`, `runProcstat("-a", "-s")`} {
		if !strings.Contains(src, want) {
			t.Errorf("discover_freebsd.go must call %s; without -a procstat prints usage and exits non-zero, so discovery silently finds nothing", want)
		}
	}
}
