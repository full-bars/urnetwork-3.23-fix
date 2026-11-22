package connect

import (
	"net"
	"strings"
	"testing"
)

// Go puts the address it was asked to dial into the error text, so an address
// that still holds a credential is printed with it. Every log site that reports
// a dial failure passes the error text through errText, which keeps the address
// and removes only the credential.
func TestErrTextRemovesTheCredentialFromADialError(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		address string
	}{
		{
			name:    "a url line with a path behind the address",
			address: "socks5://demo:secret@203.0.113.10:1080/path",
		},
		{
			name:    "a line with an empty user",
			address: ":secret@203.0.113.10:1080",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := (&net.Dialer{}).Dial("tcp", testCase.address)
			if err == nil {
				t.Fatalf("the dialer accepted %q", testCase.address)
			}
			got := errText(err)
			if strings.Contains(got, "secret") {
				t.Fatalf("the credential stayed in the error text: %q", got)
			}
			if !strings.Contains(got, "203.0.113.10:1080") {
				t.Fatalf("the address went with the credential: %q", got)
			}
		})
	}
}
