package connect

// ip_remote_multi_client_observability.go — the structured "[rel]" log line
// grammar the window's events print through: one greppable event=name line
// with lowercased key=value fields.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// relPrefix opens every structured line. The space is part of it: `grep '[rel] '`
// must not match a line that merely mentions [rel] in prose.
const relPrefix = "[rel] "

// relExitIdLength is how much of a client id the grammar carries: the last 8
// hex characters identify one exit within a session by eye and by grep.
const relExitIdLength = 8

// relEvent formats one structured event line from alternating key/value
// arguments: relEvent("quarantine", "exit", id, "flows", 3) yields
// "[rel] event=quarantine exit=a1b2c3d4 flows=3".
//
// Pure and total: it never panics, never allocates on a hot path (there is no
// call site on one), and tolerates a malformed argument list rather than
// crashing a tunnel over a log line. A trailing unpaired argument is dropped; a
// non-string key is rendered through the same value formatter so the line is
// still readable rather than missing.
func relEvent(name string, keyValues ...any) string {
	var sb strings.Builder
	sb.WriteString(relPrefix)
	sb.WriteString("event=")
	sb.WriteString(relValue(name))
	for i := 0; i+1 < len(keyValues); i += 2 {
		key, ok := keyValues[i].(string)
		if !ok {
			key = fmt.Sprintf("%v", keyValues[i])
		}
		sb.WriteString(" ")
		// keys are lowercased here rather than trusted from the call site: the
		// grammar's whole value is that `grep 'exit='` finds every exit, and one
		// capitalized key at one call site silently breaks that for the one line
		// that mattered
		sb.WriteString(strings.ToLower(key))
		sb.WriteString("=")
		sb.WriteString(relValue(keyValues[i+1]))
	}
	return sb.String()
}

// relValue renders one value in the grammar's terms: durations as millisecond
// integers (a field capture compares numbers, and "1.5s" vs "1500ms" vs "1s500ms"
// are three spellings of one number), bools as 1/0 (so a settings blob greps as
// `busyprobe=1` rather than matching the substring of `busyprobe=true` in
// `busyprobefalse`), client ids as their 8-char tail, and everything else as
// text quoted only when it could contain a space.
func relValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "-"
	case string:
		return relQuote(typed)
	case bool:
		if typed {
			return "1"
		}
		return "0"
	// time.Duration and Id are both Stringers, so they must precede the
	// Stringer case below or they would render in their own spellings
	case time.Duration:
		return strconv.FormatInt(typed.Milliseconds(), 10)
	case Id:
		return relExitId(typed)
	case int:
		return strconv.Itoa(typed)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	case int64:
		return strconv.FormatInt(typed, 10)
	case uint32:
		return strconv.FormatUint(uint64(typed), 10)
	case uint64:
		return strconv.FormatUint(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'f', 2, 64)
	case error:
		return relQuote(typed.Error())
	case fmt.Stringer:
		return relQuote(typed.String())
	default:
		return relQuote(fmt.Sprintf("%v", value))
	}
}

// relQuote quotes a value only when leaving it bare would break the
// space-separated reading of the line. Most values (reasons, states, counts)
// are single tokens and stay bare, which is what keeps the lines scannable by
// eye; errors and free text get quotes so a parser -- or the next `awk -F' '`
// -- does not silently see extra fields.
func relQuote(value string) string {
	if value == "" {
		// an empty bare value reads as a missing one; the quotes say the field
		// was present and empty, which is a different fact
		return `""`
	}
	quote := false
	for _, r := range value {
		if r == ' ' || r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			quote = true
			break
		}
	}
	if !quote {
		return value
	}
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				// other control bytes would corrupt the line in a terminal; a
				// visible escape keeps the capture readable
				sb.WriteString(fmt.Sprintf(`\x%02x`, r))
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// relExitId renders a client id as its 8-character tail. The zero id (a bare
// fixture channel, or a channel whose underlying client is gone) renders as its
// own tail rather than as an error: a log line must never be the thing that
// panics.
func relExitId(clientId Id) string {
	s := clientId.String()
	if len(s) <= relExitIdLength {
		return s
	}
	return s[len(s)-relExitIdLength:]
}
