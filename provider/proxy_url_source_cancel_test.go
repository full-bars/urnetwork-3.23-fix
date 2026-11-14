package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type cancelSourceTransport struct {
	cancel context.CancelFunc
}

func (tr cancelSourceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       &cancelSourceBody{Reader: strings.NewReader("192.0.2.1:1\n"), cancel: tr.cancel},
		Request:    req,
	}, nil
}

type cancelSourceBody struct {
	*strings.Reader
	cancel context.CancelFunc
}

func (b *cancelSourceBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		b.cancel()
	}
	return n, err
}

func (b *cancelSourceBody) Close() error { return nil }

// A shutdown after the source body arrives must not report its cancelled probe
// as a dead source. The fake transport makes cancellation deterministic and
// avoids real network I/O.
func TestFetchAndMergeProxyURLs_ShutdownMidCycleIsNotSourceFailure(t *testing.T) {
	withTempHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	previous := proxyURLHTTPClient
	proxyURLHTTPClient = &http.Client{Transport: cancelSourceTransport{cancel: cancel}}
	t.Cleanup(func() { proxyURLHTTPClient = previous })

	out := captureTlog(t, func() {
		fetchAndMergeProxyURLs(ctx, []string{"http://source.invalid/list"}, 0, "", 0)
	})
	if ctx.Err() == nil {
		t.Fatal("test did not cancel the fetch cycle")
	}
	if !strings.Contains(out, "downloaded 1 line(s)") {
		t.Fatalf("test did not reach the source probe: %s", out)
	}
	if strings.Contains(out, "all unparseable or dead") {
		t.Fatalf("shutdown reported as a source failure: %s", out)
	}
}
