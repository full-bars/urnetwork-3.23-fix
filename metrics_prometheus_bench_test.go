package connect

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// BenchmarkPrometheusRender measures one /metrics render through the real
// handler, the exact cost of the 9 percent allocation source measured on the
// canary. ReportAllocs is the point: B/op and allocs/op are the numbers that
// matter, not ns/op alone.
func BenchmarkPrometheusRender(b *testing.B) {
	handler := PrometheusHandler()

	// one warm render so lazily-started internals do not skew the first loop
	warm := httptest.NewRecorder()
	handler.ServeHTTP(warm, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		io.Discard.Write(recorder.Body.Bytes())
	}
}
