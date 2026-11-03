package connect

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestH1PlusFailureClassesTableDriven(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantAssert func(t *testing.T, snap H1PlusStatsSnapshot)
	}{
		{
			name: "timeout_deadline_exceeded",
			err:  context.DeadlineExceeded,
			wantAssert: func(t *testing.T, snap H1PlusStatsSnapshot) {
				if snap.HandshakeFailuresTimeout != 1 {
					t.Fatalf("expected HandshakeFailuresTimeout=1, got %d", snap.HandshakeFailuresTimeout)
				}
			},
		},
		{
			name: "timeout_net_error",
			err:  &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded},
			wantAssert: func(t *testing.T, snap H1PlusStatsSnapshot) {
				if snap.HandshakeFailuresTimeout != 1 {
					t.Fatalf("expected HandshakeFailuresTimeout=1, got %d", snap.HandshakeFailuresTimeout)
				}
			},
		},
		{
			name: "tls_record_header_error",
			err:  &tls.RecordHeaderError{Msg: "oversized record received"},
			wantAssert: func(t *testing.T, snap H1PlusStatsSnapshot) {
				if snap.HandshakeFailuresTls != 1 {
					t.Fatalf("expected HandshakeFailuresTls=1, got %d", snap.HandshakeFailuresTls)
				}
			},
		},
		{
			name: "tls_cert_invalid_error",
			err:  &x509.CertificateInvalidError{Reason: x509.Expired},
			wantAssert: func(t *testing.T, snap H1PlusStatsSnapshot) {
				if snap.HandshakeFailuresTls != 1 {
					t.Fatalf("expected HandshakeFailuresTls=1, got %d", snap.HandshakeFailuresTls)
				}
			},
		},
		{
			name: "tls_unknown_authority",
			err:  x509.UnknownAuthorityError{},
			wantAssert: func(t *testing.T, snap H1PlusStatsSnapshot) {
				if snap.HandshakeFailuresTls != 1 {
					t.Fatalf("expected HandshakeFailuresTls=1, got %d", snap.HandshakeFailuresTls)
				}
			},
		},
		{
			name: "proxy_connect_op_error",
			err:  &net.OpError{Op: "proxyconnect", Err: errors.New("proxy host unreachable")},
			wantAssert: func(t *testing.T, snap H1PlusStatsSnapshot) {
				if snap.HandshakeFailuresProxyConnect != 1 {
					t.Fatalf("expected HandshakeFailuresProxyConnect=1, got %d", snap.HandshakeFailuresProxyConnect)
				}
			},
		},
		{
			name: "proxy_connect_upgrade_reason",
			err:  &HTTPUpgradeError{Reason: "http-proxy", Terminal: true},
			wantAssert: func(t *testing.T, snap H1PlusStatsSnapshot) {
				if snap.HandshakeFailuresProxyConnect != 1 {
					t.Fatalf("expected HandshakeFailuresProxyConnect=1, got %d", snap.HandshakeFailuresProxyConnect)
				}
			},
		},
		{
			name: "auth_401",
			err:  &HTTPUpgradeError{StatusCode: 401, Reason: "rejected", Terminal: true},
			wantAssert: func(t *testing.T, snap H1PlusStatsSnapshot) {
				if snap.HandshakeFailuresAuth != 1 {
					t.Fatalf("expected HandshakeFailuresAuth=1, got %d", snap.HandshakeFailuresAuth)
				}
			},
		},
		{
			name: "auth_403",
			err:  &HTTPUpgradeError{StatusCode: 403, Reason: "rejected", Terminal: true},
			wantAssert: func(t *testing.T, snap H1PlusStatsSnapshot) {
				if snap.HandshakeFailuresAuth != 1 {
					t.Fatalf("expected HandshakeFailuresAuth=1, got %d", snap.HandshakeFailuresAuth)
				}
			},
		},
		{
			name: "upgrade_rejected_302",
			err:  &HTTPUpgradeError{StatusCode: 302, Reason: "rejected", Terminal: true},
			wantAssert: func(t *testing.T, snap H1PlusStatsSnapshot) {
				if snap.HandshakeFailuresUpgradeRejected != 1 {
					t.Fatalf("expected HandshakeFailuresUpgradeRejected=1, got %d", snap.HandshakeFailuresUpgradeRejected)
				}
			},
		},
		{
			name: "io_unexpected_eof",
			err:  io.ErrUnexpectedEOF,
			wantAssert: func(t *testing.T, snap H1PlusStatsSnapshot) {
				if snap.HandshakeFailuresIO != 1 {
					t.Fatalf("expected HandshakeFailuresIO=1, got %d", snap.HandshakeFailuresIO)
				}
			},
		},
		{
			name: "io_closed_pipe",
			err:  io.ErrClosedPipe,
			wantAssert: func(t *testing.T, snap H1PlusStatsSnapshot) {
				if snap.HandshakeFailuresIO != 1 {
					t.Fatalf("expected HandshakeFailuresIO=1, got %d", snap.HandshakeFailuresIO)
				}
			},
		},
		{
			name: "io_conn_reset",
			err:  syscall.ECONNRESET,
			wantAssert: func(t *testing.T, snap H1PlusStatsSnapshot) {
				if snap.HandshakeFailuresIO != 1 {
					t.Fatalf("expected HandshakeFailuresIO=1, got %d", snap.HandshakeFailuresIO)
				}
			},
		},
		{
			name: "io_read_op_error",
			err:  &net.OpError{Op: "read", Err: errors.New("underlying socket reset")},
			wantAssert: func(t *testing.T, snap H1PlusStatsSnapshot) {
				if snap.HandshakeFailuresIO != 1 {
					t.Fatalf("expected HandshakeFailuresIO=1, got %d", snap.HandshakeFailuresIO)
				}
			},
		},
		{
			name: "other_generic_error",
			err:  errors.New("unexpected non-classified error"),
			wantAssert: func(t *testing.T, snap H1PlusStatsSnapshot) {
				if snap.HandshakeFailuresOther != 1 {
					t.Fatalf("expected HandshakeFailuresOther=1, got %d", snap.HandshakeFailuresOther)
				}
			},
		},
		{
			name: "other_missing_dialer",
			err:  &HTTPUpgradeError{Reason: "missing-dialer", Terminal: true},
			wantAssert: func(t *testing.T, snap H1PlusStatsSnapshot) {
				if snap.HandshakeFailuresOther != 1 {
					t.Fatalf("expected HandshakeFailuresOther=1, got %d", snap.HandshakeFailuresOther)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stats := &H1PlusStats{}
			elapsed := 125 * time.Millisecond
			RecordH1PlusSelection(stats, elapsed, tc.err)

			snap := stats.Snapshot()
			if snap.Attempts != 1 {
				t.Fatalf("expected Attempts=1, got %d", snap.Attempts)
			}
			if snap.Accepted != 0 {
				t.Fatalf("expected Accepted=0, got %d", snap.Accepted)
			}
			if snap.Fallbacks != 0 {
				t.Fatalf("expected Fallbacks=0, got %d", snap.Fallbacks)
			}
			if snap.HandshakeFailures != 1 {
				t.Fatalf("expected HandshakeFailures=1, got %d", snap.HandshakeFailures)
			}
			if snap.HandshakeFailureNanos != uint64(elapsed) {
				t.Fatalf("expected HandshakeFailureNanos=%d, got %d", uint64(elapsed), snap.HandshakeFailureNanos)
			}

			tc.wantAssert(t, snap)

			classesSum := snap.HandshakeFailuresTimeout +
				snap.HandshakeFailuresTls +
				snap.HandshakeFailuresProxyConnect +
				snap.HandshakeFailuresAuth +
				snap.HandshakeFailuresUpgradeRejected +
				snap.HandshakeFailuresIO +
				snap.HandshakeFailuresOther
			if classesSum != snap.HandshakeFailures {
				t.Fatalf("class sum %d does not equal HandshakeFailures %d", classesSum, snap.HandshakeFailures)
			}
		})
	}
}

func TestH1PlusSelectionSuccess(t *testing.T) {
	stats := &H1PlusStats{}
	RecordH1PlusSelection(stats, 50*time.Millisecond, nil)

	snap := stats.Snapshot()
	if snap.Attempts != 1 {
		t.Fatalf("expected Attempts=1, got %d", snap.Attempts)
	}
	if snap.Accepted != 1 {
		t.Fatalf("expected Accepted=1, got %d", snap.Accepted)
	}
	if snap.Fallbacks != 0 {
		t.Fatalf("expected Fallbacks=0, got %d", snap.Fallbacks)
	}
	if snap.HandshakeFailures != 0 {
		t.Fatalf("expected HandshakeFailures=0, got %d", snap.HandshakeFailures)
	}
	if snap.HandshakeFailureNanos != 0 {
		t.Fatalf("expected HandshakeFailureNanos=0, got %d", snap.HandshakeFailureNanos)
	}

	classesSum := snap.HandshakeFailuresTimeout +
		snap.HandshakeFailuresTls +
		snap.HandshakeFailuresProxyConnect +
		snap.HandshakeFailuresAuth +
		snap.HandshakeFailuresUpgradeRejected +
		snap.HandshakeFailuresIO +
		snap.HandshakeFailuresOther
	if classesSum != 0 {
		t.Fatalf("expected all class counters to be 0 on success, got sum %d", classesSum)
	}
}

func TestH1PlusSelectionFallback(t *testing.T) {
	stats := &H1PlusStats{}
	fallbackErr := &HTTPUpgradeError{Reason: "rejected", StatusCode: 400}
	if !HTTPUpgradeAllowsFallback(fallbackErr) {
		t.Fatalf("expected error to allow fallback")
	}

	RecordH1PlusSelection(stats, 75*time.Millisecond, fallbackErr)

	snap := stats.Snapshot()
	if snap.Attempts != 1 {
		t.Fatalf("expected Attempts=1, got %d", snap.Attempts)
	}
	if snap.Accepted != 0 {
		t.Fatalf("expected Accepted=0, got %d", snap.Accepted)
	}
	if snap.Fallbacks != 1 {
		t.Fatalf("expected Fallbacks=1, got %d", snap.Fallbacks)
	}
	if snap.FallbackRejected != 1 {
		t.Fatalf("expected FallbackRejected=1, got %d", snap.FallbackRejected)
	}
	if snap.HandshakeFailures != 0 {
		t.Fatalf("expected HandshakeFailures=0, got %d", snap.HandshakeFailures)
	}
	if snap.HandshakeFailureNanos != 0 {
		t.Fatalf("expected HandshakeFailureNanos=0, got %d", snap.HandshakeFailureNanos)
	}

	classesSum := snap.HandshakeFailuresTimeout +
		snap.HandshakeFailuresTls +
		snap.HandshakeFailuresProxyConnect +
		snap.HandshakeFailuresAuth +
		snap.HandshakeFailuresUpgradeRejected +
		snap.HandshakeFailuresIO +
		snap.HandshakeFailuresOther
	if classesSum != 0 {
		t.Fatalf("expected all class counters to be 0 on fallback, got sum %d", classesSum)
	}
}

func TestH1PlusAuthVsUpgradeRejected(t *testing.T) {
	t.Run("401 increments auth and not upgrade_rejected", func(t *testing.T) {
		stats := &H1PlusStats{}
		err401 := &HTTPUpgradeError{StatusCode: 401, Reason: "rejected", Terminal: true}
		RecordH1PlusSelection(stats, 100*time.Millisecond, err401)

		snap := stats.Snapshot()
		if snap.HandshakeFailuresAuth != 1 {
			t.Fatalf("expected HandshakeFailuresAuth=1, got %d", snap.HandshakeFailuresAuth)
		}
		if snap.HandshakeFailuresUpgradeRejected != 0 {
			t.Fatalf("expected HandshakeFailuresUpgradeRejected=0, got %d", snap.HandshakeFailuresUpgradeRejected)
		}
		if snap.AuthFailures != 1 {
			t.Fatalf("expected existing AuthFailures=1, got %d", snap.AuthFailures)
		}
		if snap.HandshakeFailures != 1 {
			t.Fatalf("expected HandshakeFailures=1, got %d", snap.HandshakeFailures)
		}
	})

	t.Run("302 increments upgrade_rejected and not auth", func(t *testing.T) {
		stats := &H1PlusStats{}
		err302 := &HTTPUpgradeError{StatusCode: 302, Reason: "rejected", Terminal: true}
		RecordH1PlusSelection(stats, 100*time.Millisecond, err302)

		snap := stats.Snapshot()
		if snap.HandshakeFailuresUpgradeRejected != 1 {
			t.Fatalf("expected HandshakeFailuresUpgradeRejected=1, got %d", snap.HandshakeFailuresUpgradeRejected)
		}
		if snap.HandshakeFailuresAuth != 0 {
			t.Fatalf("expected HandshakeFailuresAuth=0, got %d", snap.HandshakeFailuresAuth)
		}
		if snap.AuthFailures != 0 {
			t.Fatalf("expected existing AuthFailures=0, got %d", snap.AuthFailures)
		}
		if snap.HandshakeFailures != 1 {
			t.Fatalf("expected HandshakeFailures=1, got %d", snap.HandshakeFailures)
		}
	})
}

func TestH1PlusFailureClassesMixedSequence(t *testing.T) {
	stats := &H1PlusStats{}

	type call struct {
		elapsed time.Duration
		err     error
	}

	sequence := []call{
		{elapsed: 10 * time.Millisecond, err: nil},
		{elapsed: 20 * time.Millisecond, err: context.DeadlineExceeded},
		{elapsed: 30 * time.Millisecond, err: &HTTPUpgradeError{Reason: "rejected", StatusCode: 400}}, // fallback
		{elapsed: 40 * time.Millisecond, err: &tls.RecordHeaderError{Msg: "bad record"}},
		{elapsed: 50 * time.Millisecond, err: nil},
		{elapsed: 60 * time.Millisecond, err: &net.OpError{Op: "proxyconnect", Err: errors.New("proxy error")}},
		{elapsed: 70 * time.Millisecond, err: &HTTPUpgradeError{StatusCode: 401, Reason: "rejected", Terminal: true}},
		{elapsed: 80 * time.Millisecond, err: &HTTPUpgradeError{StatusCode: 403, Reason: "rejected", Terminal: true}},
		{elapsed: 90 * time.Millisecond, err: &HTTPUpgradeError{StatusCode: 302, Reason: "rejected", Terminal: true}},
		{elapsed: 100 * time.Millisecond, err: io.ErrUnexpectedEOF},
		{elapsed: 110 * time.Millisecond, err: errors.New("unclassified dial failure")},
		{elapsed: 120 * time.Millisecond, err: &HTTPUpgradeError{StatusCode: 307, Reason: "rejected", Terminal: true}},
		{elapsed: 130 * time.Millisecond, err: io.EOF},
		{elapsed: 140 * time.Millisecond, err: nil},
	}

	var wantAttempts, wantAccepted, wantFallbacks, wantFailures uint64
	var wantTotalNanos, wantFailureNanos uint64

	for _, c := range sequence {
		RecordH1PlusSelection(stats, c.elapsed, c.err)
		wantAttempts++
		wantTotalNanos += uint64(c.elapsed)
		if c.err == nil {
			wantAccepted++
		} else if HTTPUpgradeAllowsFallback(c.err) {
			wantFallbacks++
		} else {
			wantFailures++
			wantFailureNanos += uint64(c.elapsed)
		}
	}

	snap := stats.Snapshot()
	if snap.Attempts != wantAttempts {
		t.Fatalf("Attempts mismatch: want %d, got %d", wantAttempts, snap.Attempts)
	}
	if snap.Accepted != wantAccepted {
		t.Fatalf("Accepted mismatch: want %d, got %d", wantAccepted, snap.Accepted)
	}
	if snap.Fallbacks != wantFallbacks {
		t.Fatalf("Fallbacks mismatch: want %d, got %d", wantFallbacks, snap.Fallbacks)
	}
	if snap.HandshakeFailures != wantFailures {
		t.Fatalf("HandshakeFailures mismatch: want %d, got %d", wantFailures, snap.HandshakeFailures)
	}
	if snap.HandshakeNanos != wantTotalNanos {
		t.Fatalf("HandshakeNanos mismatch: want %d, got %d", wantTotalNanos, snap.HandshakeNanos)
	}
	if snap.HandshakeFailureNanos != wantFailureNanos {
		t.Fatalf("HandshakeFailureNanos mismatch: want %d, got %d", wantFailureNanos, snap.HandshakeFailureNanos)
	}

	classesSum := snap.HandshakeFailuresTimeout +
		snap.HandshakeFailuresTls +
		snap.HandshakeFailuresProxyConnect +
		snap.HandshakeFailuresAuth +
		snap.HandshakeFailuresUpgradeRejected +
		snap.HandshakeFailuresIO +
		snap.HandshakeFailuresOther

	if classesSum != snap.HandshakeFailures {
		t.Fatalf("sum of class counters %d does not equal HandshakeFailures %d", classesSum, snap.HandshakeFailures)
	}
}

func TestH1PlusStatsSnapshotCopiesNewCounters(t *testing.T) {
	stats := &H1PlusStats{}

	RecordH1PlusSelection(stats, 10*time.Millisecond, context.DeadlineExceeded)
	RecordH1PlusSelection(stats, 20*time.Millisecond, &tls.RecordHeaderError{Msg: "tls fail"})
	RecordH1PlusSelection(stats, 30*time.Millisecond, &net.OpError{Op: "proxyconnect", Err: errors.New("proxy")})
	RecordH1PlusSelection(stats, 40*time.Millisecond, &HTTPUpgradeError{StatusCode: 401, Reason: "rejected", Terminal: true})
	RecordH1PlusSelection(stats, 50*time.Millisecond, &HTTPUpgradeError{StatusCode: 302, Reason: "rejected", Terminal: true})
	RecordH1PlusSelection(stats, 60*time.Millisecond, io.ErrUnexpectedEOF)
	RecordH1PlusSelection(stats, 70*time.Millisecond, errors.New("other"))

	snap := stats.Snapshot()

	if snap.HandshakeFailuresTimeout != 1 {
		t.Fatalf("snapshot HandshakeFailuresTimeout: want 1, got %d", snap.HandshakeFailuresTimeout)
	}
	if snap.HandshakeFailuresTls != 1 {
		t.Fatalf("snapshot HandshakeFailuresTls: want 1, got %d", snap.HandshakeFailuresTls)
	}
	if snap.HandshakeFailuresProxyConnect != 1 {
		t.Fatalf("snapshot HandshakeFailuresProxyConnect: want 1, got %d", snap.HandshakeFailuresProxyConnect)
	}
	if snap.HandshakeFailuresAuth != 1 {
		t.Fatalf("snapshot HandshakeFailuresAuth: want 1, got %d", snap.HandshakeFailuresAuth)
	}
	if snap.HandshakeFailuresUpgradeRejected != 1 {
		t.Fatalf("snapshot HandshakeFailuresUpgradeRejected: want 1, got %d", snap.HandshakeFailuresUpgradeRejected)
	}
	if snap.HandshakeFailuresIO != 1 {
		t.Fatalf("snapshot HandshakeFailuresIO: want 1, got %d", snap.HandshakeFailuresIO)
	}
	if snap.HandshakeFailuresOther != 1 {
		t.Fatalf("snapshot HandshakeFailuresOther: want 1, got %d", snap.HandshakeFailuresOther)
	}
	if snap.HandshakeFailures != 7 {
		t.Fatalf("snapshot HandshakeFailures: want 7, got %d", snap.HandshakeFailures)
	}
	expectedFailureNanos := uint64((10 + 20 + 30 + 40 + 50 + 60 + 70) * time.Millisecond)
	if snap.HandshakeFailureNanos != expectedFailureNanos {
		t.Fatalf("snapshot HandshakeFailureNanos: want %d, got %d", expectedFailureNanos, snap.HandshakeFailureNanos)
	}

	var nilStats *H1PlusStats
	nilSnap := nilStats.Snapshot()
	if nilSnap != (H1PlusStatsSnapshot{}) {
		t.Fatalf("expected zero snapshot for nil stats, got %+v", nilSnap)
	}
}

func TestH1PlusRealUpgradeOutcomes(t *testing.T) {
	classCounters := func(snap H1PlusStatsSnapshot) [7]uint64 {
		return [7]uint64{
			snap.HandshakeFailuresTimeout, snap.HandshakeFailuresTls, snap.HandshakeFailuresProxyConnect,
			snap.HandshakeFailuresAuth, snap.HandshakeFailuresUpgradeRejected, snap.HandshakeFailuresIO,
			snap.HandshakeFailuresOther,
		}
	}

	t.Run("501 non-terminal is a fallback and moves no class", func(t *testing.T) {
		stats := &H1PlusStats{}
		RecordH1PlusSelection(stats, 10*time.Millisecond, &HTTPUpgradeError{StatusCode: 501, Reason: "rejected", Terminal: false})
		snap := stats.Snapshot()
		if snap.Fallbacks != 1 || snap.FallbackRejected != 1 {
			t.Fatalf("expected Fallbacks=1 FallbackRejected=1, got %d %d", snap.Fallbacks, snap.FallbackRejected)
		}
		if snap.HandshakeFailures != 0 {
			t.Fatalf("expected HandshakeFailures=0, got %d", snap.HandshakeFailures)
		}
		if classCounters(snap) != [7]uint64{} {
			t.Fatalf("expected every class counter 0, got %v", classCounters(snap))
		}
	})

	t.Run("401 joined with a deadline is auth, not timeout", func(t *testing.T) {
		stats := &H1PlusStats{}
		err := errors.Join(&HTTPUpgradeError{StatusCode: 401, Reason: "rejected", Terminal: true}, context.DeadlineExceeded)
		RecordH1PlusSelection(stats, 10*time.Millisecond, err)
		snap := stats.Snapshot()
		if snap.HandshakeFailuresAuth != 1 {
			t.Fatalf("expected HandshakeFailuresAuth=1, got %d", snap.HandshakeFailuresAuth)
		}
		if snap.HandshakeFailuresTimeout != 0 {
			t.Fatalf("expected HandshakeFailuresTimeout=0, got %d", snap.HandshakeFailuresTimeout)
		}
		if snap.HandshakeFailures != 1 {
			t.Fatalf("expected HandshakeFailures=1, got %d", snap.HandshakeFailures)
		}
	})

	t.Run("302 joined with a deadline is upgrade_rejected, not timeout", func(t *testing.T) {
		stats := &H1PlusStats{}
		err := errors.Join(&HTTPUpgradeError{StatusCode: 302, Reason: "rejected", Terminal: true}, context.DeadlineExceeded)
		RecordH1PlusSelection(stats, 10*time.Millisecond, err)
		snap := stats.Snapshot()
		if snap.HandshakeFailuresUpgradeRejected != 1 || snap.HandshakeFailuresTimeout != 0 {
			t.Fatalf("expected upgrade_rejected=1 timeout=0, got %d %d", snap.HandshakeFailuresUpgradeRejected, snap.HandshakeFailuresTimeout)
		}
	})
}

// The snapshot invariant is sampled, not proven: a loop of concurrent
// recordings makes a torn total/class read overwhelmingly likely to show up
// against a snapshot that loads the total separately from the classes.
func TestH1PlusSnapshotFailureSumMatchesClasses(t *testing.T) {
	stats := &H1PlusStats{}
	errs := []error{
		context.DeadlineExceeded,
		&tls.RecordHeaderError{Msg: "bad record"},
		&net.OpError{Op: "proxyconnect", Err: errors.New("proxy")},
		&HTTPUpgradeError{StatusCode: 401, Reason: "rejected", Terminal: true},
		&HTTPUpgradeError{StatusCode: 302, Reason: "rejected", Terminal: true},
		io.ErrUnexpectedEOF,
		errors.New("other"),
	}

	const recorders, perRecorder = 8, 20000
	var wg sync.WaitGroup
	done := make(chan struct{})
	for i := 0; i < recorders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < perRecorder; n++ {
				RecordH1PlusSelection(stats, time.Millisecond, errs[(i+n)%len(errs)])
			}
		}(i)
	}
	go func() {
		wg.Wait()
		close(done)
	}()

	check := func(snap H1PlusStatsSnapshot) {
		sum := snap.HandshakeFailuresTimeout + snap.HandshakeFailuresTls + snap.HandshakeFailuresProxyConnect +
			snap.HandshakeFailuresAuth + snap.HandshakeFailuresUpgradeRejected + snap.HandshakeFailuresIO +
			snap.HandshakeFailuresOther
		if sum != snap.HandshakeFailures {
			t.Fatalf("class sum %d != HandshakeFailures %d in one snapshot", sum, snap.HandshakeFailures)
		}
	}
	for sampling := true; sampling; {
		select {
		case <-done:
			sampling = false
		default:
			check(stats.Snapshot())
		}
	}
	final := stats.Snapshot()
	check(final)
	if final.HandshakeFailures != recorders*perRecorder {
		t.Fatalf("expected %d failures, got %d", recorders*perRecorder, final.HandshakeFailures)
	}
}
