package connect

import "testing"

func TestSnPoolClaimUrl(t *testing.T) {
	api := &BringYourApi{apiUrl: "https://api.example"}
	if got := api.snPoolClaimUrl(&SnPoolClaimArgs{Epoch: 7}); got != "https://api.example/sn/pool/claim?epoch=7" {
		t.Fatalf("normal claim url = %q", got)
	}
	got := api.snPoolClaimUrl(&SnPoolClaimArgs{Epoch: 7, LegacyColdkey: "5Grw+x y"})
	if got != "https://api.example/sn/pool/claim?epoch=7&legacy_coldkey=5Grw%2Bx+y" {
		t.Fatalf("legacy claim url = %q", got)
	}
}
