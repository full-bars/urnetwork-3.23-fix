package urnettools

import (
	"strconv"
	"strings"
	"testing"
)

// The FreeBSD tuning set must contain no Linux-only key. A Linux name here
// would apply as a no-op on FreeBSD while the command still reported success,
// which is the exact failure optimizeFor used to route there.
func TestBSDSysctlWritesAreFreeBSDKeys(t *testing.T) {
	for _, w := range bsdSysctlWrites() {
		key := w[0]
		for _, linuxOnly := range []string{
			"net.core.", "net.ipv4.", "fs.file-max", "net.netfilter.",
		} {
			if strings.HasPrefix(key, linuxOnly) {
				t.Errorf("bsdSysctlWrites contains linux-only key %q", key)
			}
		}
		if !strings.HasPrefix(key, "net.inet.") && !strings.HasPrefix(key, "kern.") {
			t.Errorf("bsdSysctlWrites key %q is neither net.inet.* nor kern.*", key)
		}
		// The persisted form must be a plain decimal: sysctl.conf is consumed
		// by the boot scripts, and a value carrying anything else would either
		// be a syntax error or truncate into a comment.
		if err := validateBSDConfValue(key, w[1]); err != nil {
			t.Errorf("tuned value for %q is not persistable: %v", key, err)
		}
	}
}

// Every tuned key must appear in the rendered sysctl.conf, or the live value
// is lost at the next reboot — which is exactly the failure this file's header
// exists to prevent. An earlier version of this test asserted the equivalent
// about an rc.conf variable, on the premise that sysrc persisted these; it did
// not, and the live values reverted at boot while the command reported success.
func TestBSDSysctlConfRendersEveryKey(t *testing.T) {
	writes := bsdSysctlWrites()
	out, err := upsertBSDConfLines("", writes)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, w := range writes {
		want := renderBSDConfLine(w[0], w[1])
		if !strings.Contains(out, want) {
			t.Errorf("rendered sysctl.conf is missing %q; the setting would revert at boot", want)
		}
	}
}

// An existing assignment is replaced, not duplicated: two lines for one key
// makes the effective value depend on read order in a file this tool rewrites.
func TestUpsertBSDConfLinesReplacesRatherThanAppends(t *testing.T) {
	conf := "# site defaults\nnet.inet.tcp.recvspace=8192\nkern.maxfiles=1000\n"
	out, err := upsertBSDConfLines(conf, [][]string{{"net.inet.tcp.recvspace", "4194304"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Count(out, "net.inet.tcp.recvspace=") != 1 {
		t.Errorf("key assigned more than once:\n%s", out)
	}
	if !strings.Contains(out, "net.inet.tcp.recvspace=4194304") {
		t.Errorf("existing assignment was not replaced:\n%s", out)
	}
	// Content this tool does not manage must survive untouched.
	if !strings.Contains(out, "kern.maxfiles=1000") {
		t.Errorf("unmanaged entry was lost:\n%s", out)
	}
	if !strings.Contains(out, "# site defaults") {
		t.Errorf("comment was lost:\n%s", out)
	}
}

// Rendering twice must be idempotent, or every run grows the managed block.
func TestUpsertBSDConfLinesIsIdempotent(t *testing.T) {
	writes := bsdSysctlWrites()
	once, err := upsertBSDConfLines("", writes)
	if err != nil {
		t.Fatalf("first render: %v", err)
	}
	twice, err := upsertBSDConfLines(once, writes)
	if err != nil {
		t.Fatalf("second render: %v", err)
	}
	if once != twice {
		t.Errorf("second render differs from the first:\n--- first ---\n%s\n--- second ---\n%s", once, twice)
	}
}

// A value that is not a plain decimal must be refused rather than written: the
// boot file is sourced by shell, so a quote or a '#' would either break the boot
// or silently change what the line means.
func TestValidateBSDConfValueRejectsNonNumeric(t *testing.T) {
	for _, bad := range []string{"", "4194304 ", "4194304\n", "4194304#c", "'4194304'", "0x10", "-1", "1;rm -rf /"} {
		if err := validateBSDConfValue("net.inet.tcp.recvspace", bad); err == nil {
			t.Errorf("value %q must be rejected", bad)
		}
	}
	for _, good := range []string{"0", "4194304", "200000"} {
		if err := validateBSDConfValue("net.inet.tcp.recvspace", good); err != nil {
			t.Errorf("value %q must be accepted: %v", good, err)
		}
	}
}

// Every key in bsdSysctlWrites raises a limit, so an already-higher current
// value must survive: writing the target unconditionally LOWERED a larger
// site default (a 500k kern.maxfiles becoming 200k), live and at boot.
func TestEffectiveBSDValueNeverLowers(t *testing.T) {
	cases := []struct{ current, target, want string }{
		{"65536", "4194304", "4194304"},   // raise
		{"8388608", "4194304", "8388608"}, // already higher: keep it
		{"200000", "200000", "200000"},    // equal
		{"", "200000", "200000"},          // unreadable falls back to target
		{"garbage", "200000", "200000"},   // non-numeric falls back to target
		{" 999 ", "1", "999"},             // whitespace tolerated
	}
	for _, c := range cases {
		if got := effectiveBSDValue(c.current, c.target); got != c.want {
			t.Errorf("effectiveBSDValue(%q, %q) = %q, want %q", c.current, c.target, got, c.want)
		}
	}
}

// The socket-buffer ceiling must be written before the TCP spaces that depend
// on it: tcp_attach reserves the space, and sbreserve refuses anything above
// sb_max_adj (derived from kern.ipc.maxsockbuf). Order is the fix.
func TestBSDSysctlWritesRaisesSockbufFirst(t *testing.T) {
	writes := bsdSysctlWrites()
	idx := map[string]int{}
	for i, w := range writes {
		idx[w[0]] = i
	}
	maxsockbuf, ok := idx["kern.ipc.maxsockbuf"]
	if !ok {
		t.Fatal("kern.ipc.maxsockbuf is not in the write list; the TCP spaces can exceed the default cap and break every new socket")
	}
	for _, k := range []string{"net.inet.tcp.recvspace", "net.inet.tcp.sendspace"} {
		if i, ok := idx[k]; !ok {
			t.Errorf("%s missing from the write list", k)
		} else if i < maxsockbuf {
			t.Errorf("%s is written before kern.ipc.maxsockbuf; the ceiling must be raised first", k)
		}
	}
	// The ceiling must actually cover the TCP spaces, with the ~0.889 factor
	// sb_max_adj applies (MCLBYTES/(MSIZE+MCLBYTES)): 4 MiB of space needs
	// more than 4.5 MiB of maxsockbuf.
	var sockbufTarget, tcpTarget float64
	for _, w := range writes {
		switch w[0] {
		case "kern.ipc.maxsockbuf":
			sockbufTarget, _ = strconv.ParseFloat(w[1], 64)
		case "net.inet.tcp.recvspace", "net.inet.tcp.sendspace":
			v, _ := strconv.ParseFloat(w[1], 64)
			if v > tcpTarget {
				tcpTarget = v
			}
		}
	}
	if sockbufTarget*0.889 < tcpTarget {
		t.Errorf("kern.ipc.maxsockbuf target %v does not cover the TCP space target %v (sb_max_adj is ~0.889 of maxsockbuf)",
			sockbufTarget, tcpTarget)
	}
}
