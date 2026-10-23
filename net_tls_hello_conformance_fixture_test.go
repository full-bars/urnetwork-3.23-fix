package connect

// net_tls_hello_conformance_fixture_test.go -- the dialer and server fixtures
// the fingerprint-drift conformance bridge (net_tls_hello_conformance_export_test.go)
// needs. upstream keeps these in net_tls_hello_test.go, whose remaining
// assertions cover the hello internals (resumption, retry, verification) and
// are not part of the drift gate; only the fixture rows and the two request
// paths are ported here, unchanged in behavior.
//
// the websocket helper returns the tls connection under the websocket as this
// tree builds it. upstream wraps it in WebSocketWriteBatchConn and unwraps
// that; this tree does not batch, so the underlying conn is the tls one.

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Answers "ok" at /, and at /ws upgrades to a websocket that sends "ok".
func testTlsHelloHandler() http.Handler {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		if err := connection.WriteMessage(websocket.TextMessage, []byte("ok")); err != nil {
			return
		}
		// held until the client closes
		_, _, _ = connection.ReadMessage()
	})
	return mux
}

// A dialer kind under test: the normal dialer or one of the resilient ones.
type testTlsHelloDialer struct {
	description string
	resilient   bool
	fragment    bool
	reorder     bool
	segment     bool
}

var testTlsHelloDialers = []testTlsHelloDialer{
	{description: "normal"},
	{description: "fragment", resilient: true, fragment: true},
	{description: "reorder", resilient: true, reorder: true},
	{description: "fragment+reorder", resilient: true, fragment: true, reorder: true},
	{description: "fragment+segment", resilient: true, fragment: true, segment: true},
}

// The strategy dialer of this kind over settings, built as NewClientStrategy
// builds it.
func (self testTlsHelloDialer) clientDialer(settings *ClientStrategySettings) *clientDialer {
	dialer := &clientDialer{
		description: self.description,
		settings:    settings,
	}
	if self.resilient {
		dialer.dialTlsContext = newResilientDialTlsContext(&settings.ConnectSettings, self.fragment, self.reorder, self.segment, clientWebSocketNextProtos)
		dialer.httpDialTlsContext = newResilientDialTlsContext(&settings.ConnectSettings, self.fragment, self.reorder, self.segment, clientHttpNextProtos)
	} else {
		dialer.dialTlsContext = newNormalDialTlsContext(settings, clientWebSocketNextProtos)
		dialer.httpDialTlsContext = newNormalDialTlsContext(settings, clientHttpNextProtos)
	}
	return dialer
}

// Requests / of the server at authority through client and checks the answer.
// The body is read and closed; the response keeps its tls state.
func testTlsHelloApiRequest(t *testing.T, client *http.Client, authority string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+authority+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("api request: %s", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read api response: %s", err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("api response = %d %q, want 200 ok", response.StatusCode, body)
	}
	return response
}

// Dials the server's websocket at authority through dialer and reads its
// greeting. Returns the tls connection under the websocket, open until the
// test ends.
func testTlsHelloWebSocket(t *testing.T, dialer *clientDialer, authority string) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	connection, response, err := dialer.WsDialer(dialer.settings).DialContext(ctx, "wss://"+authority+"/ws", nil)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		t.Fatalf("websocket dial: %s", err)
	}
	t.Cleanup(func() { connection.Close() })
	if err := connection.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, message, err := connection.ReadMessage()
	if err != nil || string(message) != "ok" {
		t.Fatalf("websocket greeting = %q, %v", message, err)
	}
	return connection.UnderlyingConn()
}
