//go:build ignore

package extender

// This test of an NLayer front's admission sits with the NLayer slice: it
// needs the nlayer test helpers and a working NLayer relay (extender_nlayer.go),
// neither of which the core slice carries. The build tag keeps it out of the
// core suite; the NLayer slice drops the tag and its helpers arrive with
// extender_nlayer_test.go.

// A hop's 429 limits the hop without holding it and the front turns to its
// other hop; once every hop is limited, the front answers its own client 429
// with the shortest backoff, for a forward before its response and for a
// relayed probe after its hop dial.
func TestNLayerLimitedHop(t *testing.T) {
	// the hop that limits: one action a minute, which its first direct dial
	// spends, since the front's hop dials leave the same loopback subnet
	limitedHop, _ := newTestAdmissionFixture(t, 1, nil)
	if err := dialTestAdmission(limitedHop, connect.ExtenderCarrierTcp); err != nil {
		t.Fatal(err)
	}
	openHop := newExtenderFixture(t, "127.0.0.1", nil)
	front := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.NLayerHops = []*connect.ExtenderConfig{
			limitedHop.extenderConfig(connect.ExtenderCarrierTcp),
			openHop.extenderConfig(connect.ExtenderCarrierTcp),
		}
		// both hops are tried within one connection
		settings.NLayerAttempts = 2
	})
	connectSettings := nlayerConnectSettings(limitedHop, openHop)
	for i := 0; front.server.NLayerStats()[0].LimitedCount == 0; i += 1 {
		if 32 <= i {
			t.Fatal("the limited hop was never picked")
		}
		if _, err := getThroughNLayer(front, connect.ExtenderCarrierTcp, connectSettings); err != nil {
			t.Fatalf("dial %d: %v; %s", i, err, nlayerErrorsOf(front, limitedHop, openHop))
		}
	}
	hopStats := front.server.NLayerStats()
	if hopStats[0].FailedCount != 0 || !hopStats[0].HeldUntil.IsZero() || hopStats[0].LimitedUntil.IsZero() {
		t.Fatalf("stats = %+v, expected the hop limited and not held", hopStats[0])
	}
	// while it is limited, every connection goes to the other hop
	relaysBefore := hopStats[1].RelayCount
	for i := 0; i < 4; i += 1 {
		if _, err := getThroughNLayer(front, connect.ExtenderCarrierTcp, connectSettings); err != nil {
			t.Fatalf("dial during the limit: %v", err)
		}
	}
	if hopStats := front.server.NLayerStats(); hopStats[1].RelayCount != relaysBefore+4 || hopStats[0].LimitedCount != 1 {
		t.Fatalf("stats = %+v, expected every connection on the open hop", hopStats)
	}

	// a front whose only hop is limited
	lonely := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.NLayerHops = []*connect.ExtenderConfig{limitedHop.extenderConfig(connect.ExtenderCarrierTcp)}
	})
	// the first forward is answered before the hop dial, so it can only close
	if _, err := getThroughNLayer(lonely, connect.ExtenderCarrierTcp, limitedHop.connectSettings()); err == nil {
		t.Fatal("a forward through a limited hop reached the destination")
	}
	waitForNLayer(t, "the lonely front to limit its hop", func() bool {
		return lonely.server.NLayerStats()[0].LimitedCount == 1
	})
	remaining := time.Until(lonely.server.NLayerStats()[0].LimitedUntil)
	err := dialTestAdmission(lonely, connect.ExtenderCarrierTcp)
	var limitedErr *connect.ExtenderLimitedError
	if !errors.As(err, &limitedErr) {
		t.Fatalf("a forward through a front with every hop limited got %v, expected a limit", err)
	}
	if limitedErr.RetryAfter <= 0 || remaining+time.Second < limitedErr.RetryAfter {
		t.Fatalf("the front's retry after %s, the hop's remaining backoff %s", limitedErr.RetryAfter, remaining)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := extenderTestDial(ctx, lonely.connectSettings(), lonely.extenderConfig(connect.ExtenderCarrierTcp), &connect.ExtenderDial{
		Service: connect.ExtenderServiceProbe,
	})
	if conn != nil {
		conn.Close()
	}
	if !errors.As(err, &limitedErr) {
		t.Fatalf("a probe through a front with every hop limited got %v, expected a limit", err)
	}
}
