package urnettools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A provider with no token omits jwt_expires. A zero time would print as
// 0001-01-01T00:00:00Z and read like a real date.
func TestStatusSummaryJSONOmitsAnUnsetTokenExpiry(t *testing.T) {
	p := Provider{Unit: "a.service", User: "u", StateDir: t.TempDir(), Running: true}
	out, err := json.Marshal(buildStatusDoc(p, collectStatusData(p)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "jwt_expires") || strings.Contains(string(out), "0001-01-01") {
		t.Fatalf("an unset expiry must be omitted: %s", out)
	}
}

func TestStatusSummaryJSONKeepsATokenExpiry(t *testing.T) {
	expires := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	p := Provider{Unit: "a.service", User: "u", StateDir: t.TempDir(), Running: true, JWTExpires: expires}
	out, err := json.Marshal(buildStatusDoc(p, collectStatusData(p)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"jwt_expires":"2027-01-02T03:04:05Z"`) {
		t.Fatalf("a set expiry must be printed: %s", out)
	}
}
