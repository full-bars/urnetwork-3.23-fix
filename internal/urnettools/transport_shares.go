package urnettools

import "math/bits"

// TransportWindowShare is one transport kind's share of the payload carried
// between two readings of the live snapshot. Used is false when the kind
// carried nothing in the window, so a reader can say unused rather than 0%.
type TransportWindowShare struct {
	Kind    string
	Bytes   uint64
	Share   float64
	Percent int
	Used    bool
}

// transportKindOrder is the stable order kinds are reported in, so a bar does
// not reorder itself between polls. A kind the provider does not run is left
// out of the result.
var transportKindOrder = []struct {
	kind string
	pick func(*LiveTransports) *LiveTransport
}{
	{"h3", func(t *LiveTransports) *LiveTransport { return t.H3 }},
	{"h1_websocket", func(t *LiveTransports) *LiveTransport { return t.H1WebSocket }},
	{"h1_plus", func(t *LiveTransports) *LiveTransport { return t.H1Plus }},
	{"dns", func(t *LiveTransports) *LiveTransport { return t.DNS }},
	{"dns_pump", func(t *LiveTransports) *LiveTransport { return t.DNSPump }},
}

// windowBytes is the payload a kind carried both ways.
func (t *LiveTransport) windowBytes() uint64 {
	if t == nil {
		return 0
	}
	return t.BytesTx + t.BytesRx
}

// transportWindowShares turns two cumulative readings into the share of the
// window's payload each kind carried. A counter that went down (a removed
// proxy, a respawn) is skipped rather than read as negative traffic. Whole
// percents are apportioned by the largest remainder, so they always add up to
// exactly 100, and the earlier kind in transportKindOrder takes a tie.
func transportWindowShares(prev, cur *LiveTransports) []TransportWindowShare {
	if cur == nil {
		return nil
	}
	shares := make([]TransportWindowShare, 0, len(transportKindOrder))
	var total uint64
	for _, entry := range transportKindOrder {
		now := entry.pick(cur)
		if now == nil {
			continue
		}
		var before uint64
		if prev != nil {
			before = entry.pick(prev).windowBytes()
		}
		carried := now.windowBytes()
		var delta uint64
		if carried > before {
			delta = carried - before
		}
		shares = append(shares, TransportWindowShare{Kind: entry.kind, Bytes: delta, Used: delta > 0})
		total += delta
	}
	if total == 0 {
		return shares
	}
	percents := apportionPercents(shares, total)
	for i := range shares {
		shares[i].Share = float64(shares[i].Bytes) / float64(total)
		shares[i].Percent = percents[i]
	}
	return shares
}

// apportionPercents splits 100 whole percent over the shares so the parts sum
// to exactly 100, giving the leftover points to the largest remainders.
func apportionPercents(shares []TransportWindowShare, total uint64) []int {
	percents := make([]int, len(shares))
	remainders := make([]uint64, len(shares))
	assigned := 0
	for i, s := range shares {
		// 128-bit product so a long-lived counter cannot overflow the scale.
		hi, lo := bits.Mul64(s.Bytes, 100)
		whole, rem := bits.Div64(hi, lo, total)
		percents[i] = int(whole)
		remainders[i] = rem
		assigned += int(whole)
	}
	// At most one point per kind is left over, because each floor loses less
	// than a whole point.
	for ; assigned < 100; assigned++ {
		best := -1
		for i := range shares {
			if percents[i] >= 100 {
				continue
			}
			if best == -1 || remainders[i] > remainders[best] {
				best = i
			}
		}
		if best == -1 {
			break
		}
		percents[best]++
		remainders[best] = 0
	}
	return percents
}
