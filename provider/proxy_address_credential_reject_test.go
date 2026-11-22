package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A line that still holds a credential after every form has been tried cannot
// become a single proxy: it is rejected (every field empty) instead of being
// kept as an address, because a dial error prints the address it failed on.
// The line itself is never printed, since it is the line that holds the
// credential.
func TestParseProxyAddressRejectsACredentialItCannotSplit(t *testing.T) {
	for _, testCase := range []struct {
		name string
		in   string
	}{
		{
			name: "a url with a path behind the address",
			in:   "socks5://demo:secret@203.0.113.10:1080/path",
		},
		{
			name: "an empty user",
			in:   ":secret@203.0.113.10:1080",
		},
		{
			name: "a query behind the address",
			in:   "demo:secret@203.0.113.10:1080?token=s3cr3t",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			address, user, password := parseProxyAddress(testCase.in)
			if address != "" || user != "" || password != "" {
				t.Fatalf("parseProxyAddress(%q) = %q %q %q, want every field empty",
					testCase.in, address, user, password)
			}
		})
	}
}

// The remote list parser takes the same view: the address it returns is a bare
// host:port, so a path or a query behind it is not carried into a dial error.
func TestParseProxyURLLineRejectsACredentialItCannotSplit(t *testing.T) {
	for _, line := range []string{
		"socks5://demo:secret@203.0.113.10:1080?token=s3cr3t",
		"demo:secret@203.0.113.10:1080/s3cr3t",
		":secret@203.0.113.10:1080",
	} {
		if address, _, _, ok := parseProxyURLLine(line); ok {
			t.Fatalf("parseProxyURLLine(%q) accepted the line as %q", line, address)
		}
	}

	address, user, password, ok := parseProxyURLLine("socks5://demo:s3cr3t@203.0.113.10:1080")
	if !ok || address != "203.0.113.10:1080" || user != "demo" || password != "s3cr3t" {
		t.Fatalf("a well formed line no longer parses: %q %q %q %v", address, user, password, ok)
	}
}

// A config entry the parser rejects owns no proxy. It is dropped from the
// launch list rather than dialed as the raw line.
func TestReadProxySettingsSkipsARejectedEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".urnetwork"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := `{"servers":{"203.0.113.10:1080":"k1","socks5://demo:secret@203.0.113.20:1080/path":"k2"}}`
	if err := os.WriteFile(filepath.Join(home, ".urnetwork", "proxy"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	all := readProxySettings()
	if len(all) != 1 {
		t.Fatalf("readProxySettings returned %d settings, want 1", len(all))
	}
	if all[0].Address != "203.0.113.10:1080" {
		t.Fatalf("the wrong entry survived: %q", all[0].Address)
	}
	for _, leaked := range []string{"secret", "@"} {
		if strings.Contains(all[0].Address, leaked) {
			t.Fatalf("the address still holds %q: %q", leaked, all[0].Address)
		}
	}
}

// Go puts the address into the error text of a failed dial, so the log sites
// that report one pass it through the redactor: the address stays, the
// credential in front of it goes.
func TestRedactedErrorMessageKeepsTheDialAddress(t *testing.T) {
	for _, address := range []string{
		"socks5://demo:secret@203.0.113.10:1080/path",
		":secret@203.0.113.10:1080",
	} {
		_, err := (&net.Dialer{}).Dial("tcp", address)
		if err == nil {
			t.Fatalf("the dialer accepted %q", address)
		}
		got := redactedErrorMessage(err)
		if strings.Contains(got, "secret") {
			t.Fatalf("the credential stayed in the error text: %q", got)
		}
		if !strings.Contains(got, "203.0.113.10:1080") {
			t.Fatalf("the address went with the credential: %q", got)
		}
	}
	if got := redactedErrorMessage(nil); got != "<nil>" {
		t.Fatalf("redactedErrorMessage(nil) = %q", got)
	}
}
