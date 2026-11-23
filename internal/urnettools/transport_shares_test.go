package urnettools

import (
	"encoding/json"
	"testing"
)

func transport(kind string, bytes uint64) LiveTransport {
	return LiveTransport{BytesTx: bytes}
}

func shareOf(t *testing.T, shares []TransportWindowShare, kind string) TransportWindowShare {
	t.Helper()
	for _, s := range shares {
		if s.Kind == kind {
			return s
		}
	}
	t.Fatalf("kind %q missing from %+v", kind, shares)
	return TransportWindowShare{}
}

func percentSum(shares []TransportWindowShare) int {
	sum := 0
	for _, s := range shares {
		sum += s.Percent
	}
	return sum
}

// The share of a kind is its bytes over the whole window, and the h1 carriers
// are separate segments.
func TestTransportWindowSharesSplitByKind(t *testing.T) {
	prev := &LiveTransports{
		H1WebSocket: &LiveTransport{},
		H1Plus:      &LiveTransport{},
		H3:          &LiveTransport{},
	}
	cur := &LiveTransports{
		H1WebSocket: &LiveTransport{BytesTx: 700, BytesRx: 50},
		H1Plus:      &LiveTransport{BytesTx: 200, BytesRx: 50},
		H3:          &LiveTransport{BytesTx: 0, BytesRx: 0},
	}
	shares := transportWindowShares(prev, cur)
	if len(shares) != 3 {
		t.Fatalf("shares = %+v", shares)
	}
	if shares[0].Kind != "h3" || shares[1].Kind != "h1_websocket" || shares[2].Kind != "h1_plus" {
		t.Fatalf("order = %v %v %v", shares[0].Kind, shares[1].Kind, shares[2].Kind)
	}
	if got := shareOf(t, shares, "h1_websocket").Bytes; got != 750 {
		t.Fatalf("websocket window bytes = %d, want 750", got)
	}
	if got := shareOf(t, shares, "h3").Percent; got != 0 {
		t.Fatalf("idle h3 percent = %d, want 0", got)
	}
	if got := shareOf(t, shares, "h1_websocket").Percent; got != 75 {
		t.Fatalf("websocket percent = %d, want 75", got)
	}
	if got := shareOf(t, shares, "h1_plus").Percent; got != 25 {
		t.Fatalf("h1+ percent = %d, want 25", got)
	}
}

// Whole percents are apportioned by the largest remainder, so they always add
// up to 100.
func TestTransportWindowSharesPercentSumsToOneHundred(t *testing.T) {
	prev := &LiveTransports{H1WebSocket: &LiveTransport{}, H1Plus: &LiveTransport{}, H3: &LiveTransport{}}
	cur := &LiveTransports{
		H1WebSocket: &LiveTransport{BytesTx: 1},
		H1Plus:      &LiveTransport{BytesTx: 1},
		H3:          &LiveTransport{BytesTx: 1},
	}
	shares := transportWindowShares(prev, cur)
	if got := percentSum(shares); got != 100 {
		t.Fatalf("percent sum = %d, want 100 (%+v)", got, shares)
	}
	// Equal fractions: the earlier kind in the stable order takes the remainder.
	if shares[0].Percent != 34 || shares[1].Percent != 33 || shares[2].Percent != 33 {
		t.Fatalf("percents = %d %d %d", shares[0].Percent, shares[1].Percent, shares[2].Percent)
	}
}

// A window where nothing moved marks every enabled kind unused instead of
// dividing by zero.
func TestTransportWindowSharesIdleWindow(t *testing.T) {
	prev := &LiveTransports{
		H1WebSocket: &LiveTransport{BytesTx: 10},
		H1Plus:      &LiveTransport{BytesTx: 20},
	}
	shares := transportWindowShares(prev, prev)
	if len(shares) != 2 {
		t.Fatalf("shares = %+v", shares)
	}
	for _, s := range shares {
		if s.Used || s.Percent != 0 || s.Share != 0 {
			t.Fatalf("idle kind reported as used: %+v", s)
		}
	}
}

// A counter that went down (a respawn or a removed proxy) is skipped, not read
// as negative traffic.
func TestTransportWindowSharesSkipsDecrease(t *testing.T) {
	prev := &LiveTransports{
		H1WebSocket: &LiveTransport{BytesTx: 1000},
		H1Plus:      &LiveTransport{BytesTx: 100},
	}
	cur := &LiveTransports{
		H1WebSocket: &LiveTransport{BytesTx: 100}, // reset
		H1Plus:      &LiveTransport{BytesTx: 300},
	}
	shares := transportWindowShares(prev, cur)
	if got := shareOf(t, shares, "h1_websocket").Bytes; got != 0 {
		t.Fatalf("decrease counted as %d bytes", got)
	}
	if got := shareOf(t, shares, "h1_websocket").Used; got {
		t.Fatal("decreased kind marked used")
	}
	if got := shareOf(t, shares, "h1_plus").Percent; got != 100 {
		t.Fatalf("h1+ percent = %d, want 100", got)
	}
}

// A kind the provider does not run is not in the result, and a kind that
// appeared since the previous reading counts from zero.
func TestTransportWindowSharesOmitAbsentKinds(t *testing.T) {
	prev := &LiveTransports{H1WebSocket: &LiveTransport{BytesTx: 100}}
	cur := &LiveTransports{
		H1WebSocket: &LiveTransport{BytesTx: 150},
		DNS:         &LiveTransport{BytesRx: 50},
	}
	shares := transportWindowShares(prev, cur)
	if len(shares) != 2 {
		t.Fatalf("shares = %+v", shares)
	}
	if got := shareOf(t, shares, "dns").Bytes; got != 50 {
		t.Fatalf("dns window bytes = %d, want 50", got)
	}
	if got := percentSum(shares); got != 100 {
		t.Fatalf("percent sum = %d", got)
	}
}

// An old provider sends no block at all, so there is nothing to draw.
func TestTransportWindowSharesNilSources(t *testing.T) {
	if shares := transportWindowShares(nil, nil); shares != nil {
		t.Fatalf("nil sources gave %+v", shares)
	}
	cur := &LiveTransports{H1WebSocket: &LiveTransport{BytesTx: 5}}
	if shares := transportWindowShares(nil, cur); len(shares) != 1 {
		t.Fatalf("first reading gave %+v", shares)
	}
}

// The mirror reads the provider's wire form, which is what the traffic reply
// actually carries.
func TestLiveTrafficDecodesProviderTransports(t *testing.T) {
	body := `{"at_unix_nano":1,"billable_bytes":10,"total_bytes":20,
		"transports":{"h1_websocket":{"bytes_tx":700,"bytes_rx":50,"frames_tx":7,"frames_rx":8,"up":2},
		"h1_plus":{"bytes_tx":200,"bytes_rx":50,"frames_tx":2,"frames_rx":3,"up":1}}}`
	var traffic LiveTraffic
	if err := json.Unmarshal([]byte(body), &traffic); err != nil {
		t.Fatal(err)
	}
	if traffic.Transports == nil || traffic.Transports.H1WebSocket == nil || traffic.Transports.H1Plus == nil {
		t.Fatalf("transports = %+v", traffic.Transports)
	}
	ws := traffic.Transports.H1WebSocket
	if ws.BytesTx != 700 || ws.BytesRx != 50 || ws.FramesTx != 7 || ws.FramesRx != 8 || ws.Up != 2 {
		t.Fatalf("websocket = %+v", ws)
	}
	if traffic.Transports.H1Plus.BytesTx != 200 || traffic.Transports.H1Plus.Up != 1 {
		t.Fatalf("h1+ = %+v", traffic.Transports.H1Plus)
	}
	if traffic.Transports.H3 != nil {
		t.Fatalf("absent kind decoded as %+v", traffic.Transports.H3)
	}
}
