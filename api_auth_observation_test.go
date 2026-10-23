// The observation collector (api_auth_observation.go) never owns work, changes
// a deadline, or retains an identity.
//
// The upstream test drives these observations through AuthNetworkClientSync,
// which exercises the collector from the transport's request path. That path
// (the write trace in net_http.go and the strategy-end hook) is not on this
// line yet, so these cases drive the collector's own steps in the order the
// transport takes them: begin, trace the post, see the response, finish.
package connect

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptrace"
	"testing"
)

func authObservationTestWrote(ctx context.Context) {
	if trace := httptrace.ContextClientTrace(ctx); trace != nil && trace.WroteRequest != nil {
		trace.WroteRequest(httptrace.WroteRequestInfo{})
	}
}

// requireAuthObservationCell asserts exactly one recorded call, in the named
// phase and with the named result. Zero cells are present in every snapshot so
// a missing capability cannot read as missing data.
func requireAuthObservationCell(t *testing.T, counts *AuthNetworkClientObservations, phase, result string) {
	t.Helper()
	values := counts.Snapshot()
	if len(values) != 36 {
		t.Fatalf("observation has %d cells, want 36", len(values))
	}
	var total, matched uint64
	for _, value := range values {
		total += value.Count
		if value.Phase == phase && value.Result == result {
			matched += value.Count
		}
	}
	if total != 1 || matched != 1 {
		t.Fatalf("logical request total=%d, %s/%s=%d, want exactly one", total, phase, result, matched)
	}
}

// A call that never posts is not a post that timed out.
func TestAuthObservationNoPost(t *testing.T) {
	counts := &AuthNetworkClientObservations{}
	ctx := WithAuthNetworkClientObservations(t.Context(), counts)
	_, observation := beginAuthRequestObservation(ctx, t.Context())
	if observation == nil {
		t.Fatal("an observed context grew no observation")
	}
	observation.finish(nil, context.DeadlineExceeded)
	requireAuthObservationCell(t, counts, "no_post", "timeout")
}

// The recorded phase is the one the request reached when it ended.
func TestAuthObservationPhases(t *testing.T) {
	for _, test := range []struct {
		name   string
		wrote  bool
		answer bool
		err    error
		result *AuthNetworkClientResult
		phase  string
		want   string
	}{
		{name: "before the write", err: context.Canceled, phase: "pre_write", want: "canceled"},
		{name: "waiting for the response", wrote: true, err: context.DeadlineExceeded, phase: "response_wait", want: "timeout"},
		{name: "reading the body", wrote: true, answer: true, err: errors.New("connection reset"), phase: "body_read", want: "error"},
		{
			name:   "an application error inside the pack",
			wrote:  true,
			answer: true,
			result: &AuthNetworkClientResult{Error: &AuthNetworkClientError{Message: "no seats"}},
			phase:  "body_read",
			want:   "api_error",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			counts := &AuthNetworkClientObservations{}
			ctx := WithAuthNetworkClientObservations(t.Context(), counts)
			ctx, observation := beginAuthRequestObservation(ctx, t.Context())
			ctx, progress := traceAuthPostAttempt(ctx)
			if progress == nil {
				t.Fatal("a traced post made no progress cell")
			}
			if test.wrote {
				authObservationTestWrote(ctx)
			}
			if test.answer {
				progress.responseHeaders(&http.Response{StatusCode: http.StatusOK}, nil)
			}
			observation.finish(test.result, test.err)
			requireAuthObservationCell(t, counts, test.phase, test.want)
		})
	}
}

// An HTTP failure keeps its class, so a token that was refused reads
// differently from a server that was rate limiting or broken.
func TestAuthObservationHttpResults(t *testing.T) {
	for _, test := range []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "http_auth"},
		{http.StatusForbidden, "http_auth"},
		{http.StatusTooManyRequests, "http_rate"},
		{http.StatusInternalServerError, "http_error"},
	} {
		counts := &AuthNetworkClientObservations{}
		ctx := WithAuthNetworkClientObservations(t.Context(), counts)
		ctx, observation := beginAuthRequestObservation(ctx, t.Context())
		ctx, progress := traceAuthPostAttempt(ctx)
		authObservationTestWrote(ctx)
		progress.responseHeaders(&http.Response{StatusCode: test.status}, nil)
		observation.finish(nil, &HttpStatusError{StatusCode: test.status, Status: http.StatusText(test.status)})
		requireAuthObservationCell(t, counts, "body_read", test.want)
	}
}

// Malformed JSON is a decode failure, not an unknown error.
func TestAuthObservationDecodeResult(t *testing.T) {
	counts := &AuthNetworkClientObservations{}
	ctx := WithAuthNetworkClientObservations(t.Context(), counts)
	ctx, observation := beginAuthRequestObservation(ctx, t.Context())
	ctx, progress := traceAuthPostAttempt(ctx)
	authObservationTestWrote(ctx)
	progress.responseHeaders(&http.Response{StatusCode: http.StatusOK}, nil)
	observation.finish(nil, &json.SyntaxError{Offset: 1})
	requireAuthObservationCell(t, counts, "body_read", "decode_error")
}

// The operation's own verdict outranks an earlier attempt's cause, so a
// canceled strategy is reported as canceled even when the error is a deadline.
func TestAuthObservationStrategyEndOutranksTheError(t *testing.T) {
	counts := &AuthNetworkClientObservations{}
	strategyCtx, cancel := context.WithCancel(t.Context())
	ctx := WithAuthNetworkClientObservations(t.Context(), counts)
	ctx, observation := beginAuthRequestObservation(ctx, t.Context())
	// the strategy's own verdict is read at request end, after the cancel
	cancel()
	observeAuthStrategyEnd(ctx, strategyCtx)
	observation.finish(nil, context.DeadlineExceeded)
	requireAuthObservationCell(t, counts, "no_post", "canceled")
}

// The api's construction context is consulted when its caller uses another
// one, so a collector set once is seen by every later call.
func TestAuthObservationOwnerContext(t *testing.T) {
	counts := &AuthNetworkClientObservations{}
	ownerCtx := WithAuthNetworkClientObservations(t.Context(), counts)
	_, observation := beginAuthRequestObservation(t.Context(), ownerCtx)
	if observation == nil {
		t.Fatal("the owner's collector was not consulted")
	}
	observation.finish(nil, context.DeadlineExceeded)
	requireAuthObservationCell(t, counts, "no_post", "timeout")
}

// Observation is opt-in: no collector means no work and no panic, and a nil
// collector is never installed.
func TestAuthObservationIsOptIn(t *testing.T) {
	if ctx := WithAuthNetworkClientObservations(t.Context(), nil); ctx != t.Context() {
		t.Fatal("a nil collector changed the context")
	}
	ctx, observation := beginAuthRequestObservation(t.Context(), t.Context())
	if observation != nil {
		t.Fatal("an unobserved context grew an observation")
	}
	if traceCtx, progress := traceAuthPostAttempt(ctx); progress != nil || traceCtx != ctx {
		t.Fatal("an unobserved post grew a progress cell")
	}
	var nilCounts *AuthNetworkClientObservations
	nilCounts.record(authPreWrite, authOK)
	for _, value := range nilCounts.Snapshot() {
		if value.Count != 0 {
			t.Fatalf("%s/%s counted without a collector", value.Phase, value.Result)
		}
	}
	var nilObservation *authRequestObservation
	nilObservation.finish(nil, nil)
}

// The snapshot is a stable four-by-nine grid in label order, whatever the
// counts, so a reader can index it without a map.
func TestAuthObservationSnapshotShape(t *testing.T) {
	counts := &AuthNetworkClientObservations{}
	ctx := WithAuthNetworkClientObservations(t.Context(), counts)
	_, observation := beginAuthRequestObservation(ctx, t.Context())
	observation.finish(nil, nil)
	values := counts.Snapshot()
	if len(values) != 36 {
		t.Fatalf("cells = %d, want 36", len(values))
	}
	for i, value := range values {
		if want := authRequestPhaseLabels[i/len(authRequestResultLabels)]; value.Phase != want {
			t.Fatalf("cell %d has phase %q, want %q", i, value.Phase, want)
		}
		if want := authRequestResultLabels[i%len(authRequestResultLabels)]; value.Result != want {
			t.Fatalf("cell %d has result %q, want %q", i, value.Result, want)
		}
	}
}
