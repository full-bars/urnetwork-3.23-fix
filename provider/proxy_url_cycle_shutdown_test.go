package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type shutdownCycleTransport struct {
	cancel    context.CancelCauseFunc
	mode      string
	laterHits int
	hits      int
}

func (tr *shutdownCycleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.hits++
	if req.URL.Path != "/first" {
		tr.laterHits++
	}
	if tr.mode == "error" && req.URL.Path == "/first" {
		tr.cancel(errors.New("test shutdown"))
		return nil, context.Canceled
	}
	body := io.ReadCloser(io.NopCloser(strings.NewReader("192.0.2.1:1\n")))
	if tr.mode == "healthy" {
		body = io.NopCloser(strings.NewReader("invalid\n"))
	}
	if tr.mode == "body" && req.URL.Path == "/first" {
		body = &shutdownCycleBody{Reader: strings.NewReader("192.0.2.1:1\n"), cancel: tr.cancel}
	}
	if tr.mode == "healthy" && req.URL.Path == "/broken" {
		return &http.Response{StatusCode: 500, Header: make(http.Header), Body: body, Request: req}, nil
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, Request: req}, nil
}

type shutdownCycleBody struct {
	*strings.Reader
	cancel context.CancelCauseFunc
}

func (b *shutdownCycleBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		b.cancel(errors.New("test shutdown"))
	}
	return n, err
}
func (b *shutdownCycleBody) Close() error { return nil }

func prepareShutdownCycle(t *testing.T, mode string) (context.Context, *shutdownCycleTransport) {
	t.Helper()
	withTempHome(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	tr := &shutdownCycleTransport{cancel: cancel, mode: mode}
	oldClient := proxyURLHTTPClient
	proxyURLHTTPClient = &http.Client{Transport: tr}
	oldStatus, oldReason := proxyResolutionStatus.Load(), getProxyResolutionReason()
	proxyResolutionStatus.Store(proxyResolutionPending)
	proxySourceWarnings.mu.Lock()
	oldWarn := proxySourceWarnings.lastWarn
	proxySourceWarnings.lastWarn = time.Time{}
	proxySourceWarnings.mu.Unlock()
	t.Cleanup(func() {
		proxyURLHTTPClient = oldClient
		proxyResolutionStatus.Store(oldStatus)
		proxyResolutionReason.Store(oldReason)
		proxySourceWarnings.mu.Lock()
		proxySourceWarnings.lastWarn = oldWarn
		proxySourceWarnings.mu.Unlock()
	})
	return ctx, tr
}

func TestFetchAndMergeProxyURLs_FullCycleShutdown(t *testing.T) {
	for _, mode := range []string{"error", "body"} {
		t.Run(mode, func(t *testing.T) {
			ctx, tr := prepareShutdownCycle(t, mode)
			out := captureTlog(t, func() {
				fetchAndMergeProxyURLs(ctx, []string{"http://source.invalid/first", "http://source.invalid/later-a", "http://source.invalid/later-b"}, 0, "", 0)
			})
			if ctx.Err() == nil {
				t.Fatal("shutdown not triggered")
			}
			t.Logf("mode=%s first-and-later-roundtrips=%d laterHits=%d", mode, tr.hits, tr.laterHits)
			if tr.laterHits != 0 {
				t.Errorf("later sources fetched: %d", tr.laterHits)
			}
			if strings.Contains(out, "fetching source.invalid/later") {
				t.Errorf("later fetch attempted: %s", out)
			}
			proxySourceWarnings.mu.Lock()
			warned := !proxySourceWarnings.lastWarn.IsZero()
			proxySourceWarnings.mu.Unlock()
			if warned {
				t.Error("source-failure warning fired during shutdown")
			}
			if proxyResolutionStatus.Load() == proxyResolutionFailed {
				t.Errorf("resolution failed during shutdown: %s", getProxyResolutionReason())
			}
			if !strings.Contains(out, "fetch cycle cancelled") || !strings.Contains(out, "test shutdown") {
				t.Errorf("missing cancellation cause: %s", out)
			}
		})
	}
}

func TestFetchAndMergeProxyURLs_FullCycleManySources(t *testing.T) {
	ctx, tr := prepareShutdownCycle(t, "healthy")
	urls := []string{"http://source.invalid/first", "http://source.invalid/broken", "http://source.invalid/a", "http://source.invalid/b"}
	captureTlog(t, func() { fetchAndMergeProxyURLs(ctx, urls, 0, "", 0) })
	if tr.hits != len(urls) {
		t.Errorf("fetched %d of %d sources", tr.hits, len(urls))
	}
}
