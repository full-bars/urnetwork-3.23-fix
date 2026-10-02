package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A source URL that fails to parse makes http.NewRequestWithContext return a
// *url.Error whose text embeds the whole URL, query token included. That error
// reaches the log, the resolution reason and the operator warning.
func TestFetchProxyURLLinesBuildRequestErrorDoesNotEchoTheURL(t *testing.T) {
	_, err := fetchProxyURLLines(context.Background(), "http://127.0.0.1:1/p%zz.txt?token=SECRET")
	if err == nil {
		t.Fatal("expected an error for a malformed URL")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error text leaks the URL's token: %q", err.Error())
	}
}

func TestFetchCycleDoesNotLogTheRawURLOfAMalformedSource(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()
	out := captureTlog(t, func() {
		fetchAndMergeProxyURLs(context.Background(), []string{"http://127.0.0.1:1/p%zz.txt?token=SECRET"}, 0, "127.0.0.1", 1)
	})
	if strings.Contains(out, "SECRET") {
		t.Fatalf("the cycle's log output leaks the URL's token:\n%s", out)
	}
}

// When a source fetches fine but every new line is dead, the cycle prints the
// "all unparseable or dead" and "probed" lines. They used the raw URL, so a
// token in the query string was written to the log.
func TestFetchCycleDeadSourceLinesUseTheRedactedLabel(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("127.0.0.1:1:user:pass\n")) // nothing listens on port 1: dead
	}))
	defer srv.Close()

	out := captureTlog(t, func() {
		fetchAndMergeProxyURLs(context.Background(), []string{srv.URL + "/dead.txt?token=SECRET"}, 0, "127.0.0.1", 1)
	})
	if !strings.Contains(out, "all unparseable or dead") {
		t.Fatalf("test setup did not reach the all-dead path:\n%s", out)
	}
	if strings.Contains(out, "SECRET") {
		t.Fatalf("the cycle's log output leaks the URL's token:\n%s", out)
	}
}
