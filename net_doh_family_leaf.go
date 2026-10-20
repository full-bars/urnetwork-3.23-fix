package connect

// DoH family-pairing cluster extracted verbatim from upstream net_http_doh.go
// at 20b29f79 (knownDohUrlPairs through dohLaunchWaves) for the udp-candidates
// leaf tests. Merge when the L1 DoH family work lands.

import "maps"

func dohUrlsFor(ipv4 []string, ipv6 []string, ipVersion int) []string {
	switch ipVersion {
	case 4:
		return ipv4
	case 6:
		return ipv6
	default:
		// both families, operator-paired (see dualStackList)
		return dualStackList(ipv4, ipv6, knownDohUrlPairs)
	}
}

// remoteDohUrls/localDohUrls return the configured wire-format DoH server URLs for the ip version.
func remoteDohUrls(settings *DohSettings, ipVersion int) []string {
	rs := settings.DnsResolverSettings
	return dohUrlsFor(rs.RemoteDohUrlsIpv4, rs.RemoteDohUrlsIpv6, ipVersion)
}

func localDohUrls(settings *DohSettings, ipVersion int) []string {
	rs := settings.DnsResolverSettings
	return dohUrlsFor(rs.LocalDohUrlsIpv4, rs.LocalDohUrlsIpv6, ipVersion)
}

var knownDohUrlPairs = [][2]string{
	{"https://1.1.1.1/dns-query", "https://[2606:4700:4700::1111]/dns-query"},
	{"https://8.8.8.8/resolve", "https://[2001:4860:4860::8888]/resolve"},
	{"https://9.9.9.9/dns-query", "https://[2620:fe::fe]/dns-query"},
	{"https://208.67.222.222/dns-query", "https://[2620:119:35::35]/dns-query"},
}

// knownDnsServerPairs are the v4/v6 pairs of the default and regional plain
// dns operators, the same rule for the plain-dns lists.
var knownDnsServerPairs = [][2]string{
	{"1.1.1.1", "2606:4700:4700::1111"},
	{"1.0.0.1", "2606:4700:4700::1001"},
	{"8.8.8.8", "2001:4860:4860::8888"},
	{"8.8.4.4", "2001:4860:4860::8844"},
	{"9.9.9.9", "2620:fe::fe"},
	{"149.112.112.112", "2620:fe::9"},
	{"208.67.222.222", "2620:119:35::35"},
	{"208.67.220.220", "2620:119:53::53"},
	{"223.5.5.5", "2400:3200::1"},
	{"119.29.29.29", "2402:4e00::"},
	{"77.88.8.8", "2a02:6b8::feed:0ff"},
}

// familySiblings pairs the entries of a v4 list and a v6 list by operator:
// a known pair from the table when both members are configured, else by
// list index for entries that are outside the table on both sides. The
// result maps each paired entry to its sibling in both directions.
//
// The table rule is what lets "start from the defaults and replace the v4
// list" keep meaning "use only these servers": the default v6 entries are
// known operators whose v4 partners are then absent, so they pair with
// nothing and dualStackList leaves them out, instead of index-pairing a
// caller's private server with Cloudflare's v6 endpoint.
func familySiblings(ipv4 []string, ipv6 []string, known [][2]string) map[string]string {
	siblings := map[string]string{}
	knownOf := map[string]string{}
	for _, pair := range known {
		knownOf[pair[0]] = pair[1]
		knownOf[pair[1]] = pair[0]
	}
	present6 := map[string]bool{}
	for _, entry := range ipv6 {
		present6[entry] = true
	}
	unknown4 := []string{}
	for _, entry := range ipv4 {
		if partner, ok := knownOf[entry]; ok {
			if present6[partner] && partner != entry {
				siblings[entry] = partner
				siblings[partner] = entry
			}
			continue
		}
		unknown4 = append(unknown4, entry)
	}
	unknown6 := []string{}
	for _, entry := range ipv6 {
		if _, ok := knownOf[entry]; !ok {
			unknown6 = append(unknown6, entry)
		}
	}
	for i := 0; i < min(len(unknown4), len(unknown6)); i++ {
		if unknown4[i] == "" || unknown6[i] == "" || unknown4[i] == unknown6[i] {
			continue
		}
		siblings[unknown4[i]] = unknown6[i]
		siblings[unknown6[i]] = unknown4[i]
	}
	return siblings
}

// dualStackList is the list a family-agnostic (IpVersion 0) consumer walks:
// every v4 entry in order, each followed by its v6 sibling, so the families
// interleave by operator.
//
// Two kinds of leftover v6 entry are treated differently, and the difference
// is what makes "start from the defaults and replace the v4 list" behave:
//
//   - A KNOWN-TABLE entry whose v4 partner is absent is left out. Those are
//     the shipped defaults; a caller who replaced the v4 list means "use my
//     servers", and pulling Cloudflare's v6 endpoint back in would send real
//     queries to an operator they just removed.
//   - An entry OUTSIDE the table is kept, at the end. Those can only have come
//     from the caller, so dropping one would ignore a server they explicitly
//     configured -- which is what happened to every custom v6 endpoint past
//     the shorter list's length, since familySiblings can only index-pair
//     min(len4, len6) of them.
func dualStackList(ipv4 []string, ipv6 []string, known [][2]string) []string {
	if len(ipv4) == 0 {
		return append([]string{}, ipv6...)
	}
	inTable := make(map[string]bool, 2*len(known))
	for _, pair := range known {
		inTable[pair[0]] = true
		inTable[pair[1]] = true
	}
	siblings := familySiblings(ipv4, ipv6, known)
	list := make([]string, 0, len(ipv4)+len(ipv6))
	emitted := make(map[string]bool, len(ipv4)+len(ipv6))
	emit := func(entry string) {
		if emitted[entry] {
			return
		}
		emitted[entry] = true
		list = append(list, entry)
	}
	for _, entry := range ipv4 {
		emit(entry)
		if sibling, ok := siblings[entry]; ok {
			emit(sibling)
		}
	}
	for _, entry := range ipv6 {
		if !inTable[entry] {
			emit(entry)
		}
	}
	return list
}

// dohServerSiblings is the sibling map over both DoH url lists, for the
// launch waves.
func dohServerSiblings(rs *DnsResolverSettings) map[string]string {
	siblings := map[string]string{}
	if rs == nil {
		return siblings
	}
	maps.Copy(siblings, familySiblings(rs.RemoteDohUrlsIpv4, rs.RemoteDohUrlsIpv6, knownDohUrlPairs))
	maps.Copy(siblings, familySiblings(rs.LocalDohUrlsIpv4, rs.LocalDohUrlsIpv6, knownDohUrlPairs))
	return siblings
}

// dohLaunchWaves groups an ordered server list into launch waves: a server
// and its operator sibling (when both are in the list) form one wave, so the
// operator's v4 and v6 endpoints race each other inside the wave instead of
// the v6 endpoint waiting a whole stagger behind the v4 one. Wave order
// follows the first appearance of either member in the weighted order, so a
// well-scoring operator still fires first. Every url appears exactly once.
func dohLaunchWaves(ordered []string, siblings map[string]string) [][]string {
	present := make(map[string]bool, len(ordered))
	for _, dohUrl := range ordered {
		present[dohUrl] = true
	}
	placed := make(map[string]bool, len(ordered))
	waves := make([][]string, 0, len(ordered))
	for _, dohUrl := range ordered {
		if placed[dohUrl] {
			continue
		}
		wave := []string{dohUrl}
		placed[dohUrl] = true
		if sibling, ok := siblings[dohUrl]; ok && present[sibling] && !placed[sibling] {
			wave = append(wave, sibling)
			placed[sibling] = true
		}
		waves = append(waves, wave)
	}
	return waves
}

// beginQuery admits one logical lookup into the shared quiet-query counter.
// The counter is shared by parsed A/AAAA and opaque SVCB/HTTPS lookups so a
// browser's three-record origin burst gets one predictable hedge allowance,
// rather than one allowance per implementation path.
