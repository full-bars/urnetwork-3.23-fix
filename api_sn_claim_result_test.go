package connect

import (
	"encoding/json"
	"testing"
)

// The server sends a claim refusal as HTTP 200 with {"error":{...}} and no
// claim fields. The client must keep the message instead of decoding an empty
// claim.
func TestSnPoolClaimResultKeepsServerRefusal(t *testing.T) {
	var refused SnPoolClaimResult
	if err := json.Unmarshal([]byte(`{"error":{"message":"No claimable epoch."}}`), &refused); err != nil {
		t.Fatal(err)
	}
	if refused.Error == nil || refused.Error.Message != "No claimable epoch." {
		t.Fatalf("refusal message was dropped: %+v", refused.Error)
	}

	var ok SnPoolClaimResult
	if err := json.Unmarshal([]byte(`{"epoch":7,"no_id":"AQ==","share_bps":10}`), &ok); err != nil {
		t.Fatal(err)
	}
	if ok.Error != nil || ok.Epoch != 7 || len(ok.NoId) != 1 {
		t.Fatalf("a normal claim must decode with no error: %+v", ok)
	}
}
