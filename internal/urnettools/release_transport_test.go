package urnettools

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingTransport answers every request with a canned release JSON and
// records the requested URLs, so the release fetch paths can be exercised
// without the network.
type recordingTransport struct {
	mu   sync.Mutex
	urls []string
	body string
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.urls = append(rt.urls, req.URL.String())
	rt.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(rt.body)),
		Request:    req,
	}, nil
}

// withRecordingTransport swaps http.DefaultTransport for a recordingTransport
// serving body until the test ends. It also clears the latest-release cache
// and restores it afterwards: a release cached earlier would bypass the
// transport, and one cached here would leak the canned tag into later tests.
func withRecordingTransport(t *testing.T, body string) *recordingTransport {
	t.Helper()
	rt := &recordingTransport{body: body}
	prev := http.DefaultTransport
	http.DefaultTransport = rt
	latestReleaseMu.Lock()
	prevInfo, prevTime := cachedLatest, cachedLatestTime
	cachedLatest, cachedLatestTime = nil, time.Time{}
	latestReleaseMu.Unlock()
	t.Cleanup(func() {
		http.DefaultTransport = prev
		latestReleaseMu.Lock()
		cachedLatest, cachedLatestTime = prevInfo, prevTime
		latestReleaseMu.Unlock()
	})
	return rt
}
