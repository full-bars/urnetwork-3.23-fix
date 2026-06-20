package connect

import (
	"strings"
	"testing"
)

// TestHttpErrorFromResponse_HTMLBodyCollapsed is a regression test for log
// spam observed during a live deployment test: when a front-end proxy
// (nginx) returns a 429/5xx with an HTML error page as the body, embedding
// it verbatim produced multi-line, unreadable error messages at every call
// site that logged the error.
func TestHttpErrorFromResponse_HTMLBodyCollapsed(t *testing.T) {
	htmlBody := []byte("<html>\n<head><title>429 Too Many Requests</title></head>\n<body>\n<center><h1>429 Too Many Requests</h1></center>\n<hr><center>nginx</center>\n</body>\n</html>")

	err := httpErrorFromResponse("429 Too Many Requests", htmlBody)
	if err == nil {
		t.Fatal("expected a non-nil error")
	}
	if strings.Contains(err.Error(), "<html>") || strings.Contains(err.Error(), "\n") {
		t.Fatalf("expected HTML body to be collapsed, got: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "429 Too Many Requests") {
		t.Fatalf("expected status to be preserved, got: %q", err.Error())
	}
}

// TestHttpErrorFromResponse_PlainBodyPreserved ensures normal (non-HTML)
// error bodies, like the JSON error messages the bringyour.com API normally
// returns, are passed through unchanged.
func TestHttpErrorFromResponse_PlainBodyPreserved(t *testing.T) {
	err := httpErrorFromResponse("400 Bad Request", []byte(`{"error":"invalid token"}`))
	if err == nil {
		t.Fatal("expected a non-nil error")
	}
	want := `400 Bad Request: {"error":"invalid token"}`
	if err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
}
