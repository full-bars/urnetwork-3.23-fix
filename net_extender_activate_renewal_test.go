package connect

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

func TestExtenderSignedRenewalLoop(t *testing.T) {
	fixture := newTestActivatorFixtureWithOperator(t, func(operator *testActivateOperator) {
		operator.recordLifetime = 14 * 24 * time.Hour
	}, nil)
	fixture.waitFirstPass()
	fixture.waitActivation()
	fixture.waitActivation()

	// The fixture signs a 14-day record. The old fixed-day tick posts here.
	fixture.step(24 * time.Hour)
	fixture.expectFamilyPostCounts("after one day", 1, 1)
	fixture.step(13*24*time.Hour - 2*time.Hour)
	fixture.expectFamilyPostCounts("one hour before renewal", 1, 1)
	fixture.clock.advance(time.Hour)
	fixture.wakeForPost()
	fixture.waitPost()
	fixture.waitActivation()
	fixture.waitActivation()
	fixture.step(time.Minute)
	fixture.expectFamilyPostCounts("at signed renewal", 2, 2)
}

func TestExtenderSignedRenewalSchedule(t *testing.T) {
	root, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1800000000, 0)
	settings := DefaultExtenderActivatorSettings()
	settings.PublicKey = public
	settings.Directory = NewExtenderDirectory(context.Background(), DefaultExtenderDirectorySettings())
	settings.Directory.SetRootKeys(NewExtenderRootKeySet(root))
	activator := &ExtenderActivator{settings: settings}
	sign := func(issued, expiry time.Time, key ed25519.PrivateKey) string {
		t.Helper()
		record, err := SignExtenderRecord(key, &protocol.ExtenderRecordBody{PublicKey: public, IssueTimeMs: uint64(issued.UnixMilli()), ExpireTimeMs: uint64(expiry.UnixMilli())})
		if err != nil {
			t.Fatal(err)
		}
		data, err := proto.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(data)
	}
	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	long := sign(now.Add(-2*time.Hour), now.Add(10*time.Hour), private)
	short := sign(now.Add(-10*time.Minute), now.Add(30*time.Minute), private)
	tests := []struct {
		name, record string
		want         time.Time
	}{
		{"long signed lifetime", long, now.Add(9 * time.Hour)},
		{"short signed lifetime", short, now.Add(20 * time.Minute)},
		{"expired", sign(now.Add(-3*time.Hour), now.Add(-time.Hour), private), now.Add(settings.ActivateTimeout)},
		{"unverified", sign(now.Add(-time.Hour), now.Add(10*time.Hour), other), now.Add(settings.ActivateTimeout)},
		{"missing", "", now.Add(settings.ActivateTimeout)},
		{"zero lifetime", sign(now.Add(time.Hour), now.Add(time.Hour), private), now.Add(settings.ActivateTimeout)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The control ensures that reverting scheduling to the fixed day breaks
			// every case, including those which must themselves use the fallback.
			for _, check := range []struct {
				record string
				want   time.Time
			}{{long, now.Add(9 * time.Hour)}, {tt.record, tt.want}} {
				schedule := &extenderActivateSchedule{}
				unsignedExpiry := now.Add(90 * 24 * time.Hour)
				point, _ := activator.renewalTime(&ExtenderActivateResult{Record: check.record, ExpireTime: &unsignedExpiry}, now)
				schedule.tick(now, settings, point)
				if !schedule.nextActivateTime.Equal(check.want) {
					t.Fatalf("next = %s, want %s", schedule.nextActivateTime, check.want)
				}
			}
		})
	}
}
