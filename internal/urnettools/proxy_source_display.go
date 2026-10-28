package urnettools

import (
	"net"
	"net/url"
	"regexp"
	"strings"
)

var sourceDisplayHostPort = regexp.MustCompile(`^[A-Za-z0-9.-]+(?::[A-Za-z0-9-]+)?$`)

// sanitizeSourceForDisplay keeps only the scheme and host of a source URL.
func sanitizeSourceForDisplay(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" && u.Hostname() != "" && !strings.Contains(u.Opaque, "@") {
		return u.Scheme + "://" + u.Host
	}
	// Strip query and fragment before finding userinfo, since a token can
	// itself contain an @ sign.
	out := raw
	if q := strings.IndexAny(out, "?#"); q >= 0 {
		out = out[:q]
	}
	hadUserinfo := false
	if at := strings.LastIndex(out, "@"); at >= 0 {
		hadUserinfo = true
		out = out[at+1:]
	}
	out = strings.TrimSpace(out)
	if strings.HasPrefix(out, "[") {
		end := strings.Index(out, "]")
		if end < 0 || net.ParseIP(out[1:end]) == nil {
			return "<unparsed source>"
		}
		port := out[end+1:]
		if port != "" && (len(port) < 2 || port[0] != ':' || !sourceDisplayDigits(port[1:])) {
			return "<unparsed source>"
		}
		return out
	}
	if !sourceDisplayHostPort.MatchString(out) {
		return "<unparsed source>"
	}
	if colon := strings.IndexByte(out, ':'); colon >= 0 && !hadUserinfo && !sourceDisplayDigits(out[colon+1:]) {
		return "<unparsed source>"
	}
	return out
}

func sourceDisplayDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
