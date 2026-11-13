package connect

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestApiGeneratorFamilyDiscovery(t *testing.T) {
	v6, legacy := NewId(), NewId()
	api := walletMappingTestApi(t, "synthetic-token", func(w http.ResponseWriter, r *http.Request) {
		var args FindProviders2Args
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil || args.IpFamily != IpFamilyFilterV6Capable {
			t.Errorf("family request: %+v, %v", args, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(FindProviders2Result{Providers: []*FindProvidersProvider{
			{ClientId: v6, IpFamily: IpFamilyDualstack},
			{ClientId: legacy},
		}})
	})
	generator := &ApiMultiClientGenerator{api: api}
	destinations, err := generator.NextDestinationsWithIpFamily(2, nil, "", IpFamilyFilterV6Capable)
	if err != nil {
		t.Fatal(err)
	}
	v6Destination, _ := NewMultiHopId(v6)
	legacyDestination, _ := NewMultiHopId(legacy)
	if !destinations[v6Destination].IpFamily.SupportsIpv6() || destinations[legacyDestination].IpFamily.SupportsIpv6() {
		t.Fatalf("wrong family labels: %+v", destinations)
	}
}
