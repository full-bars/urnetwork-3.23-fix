package main

import (
	"strings"
	"testing"
	"time"
)

func TestDiscoveryLogsDescribeProgressWithoutSpam(t *testing.T) {
	var s discoveryLogState
	now := time.Unix(1000, 0)
	check := func(d extenderDiscoverySnapshot, at time.Time, want string) {
		t.Helper()
		got := strings.Join(s.lines(d, at), "")
		if !strings.Contains(got, want) {
			t.Fatalf("log %q, want %q", got, want)
		}
	}
	d := extenderDiscoverySnapshot{State: extenderDiscoveryStateWaiting}
	check(d, now, "waiting for the platform")
	if got := s.lines(d, now.Add(time.Second)); len(got) != 0 {
		t.Fatalf("repeated waiting: %v", got)
	}
	d.State = extenderDiscoveryStateRunning
	var messages, errors uint64
	d.MessagesAccepted, d.ApplyErrors = &messages, &errors
	check(d, now.Add(time.Second), "no mesh peers yet")
	d.Peers = 3
	check(d, now.Add(2*time.Second), "3 mesh peer")
	messages = 1
	check(d, now.Add(3*time.Second), "received 1 valid message")
	if got := s.lines(d, now.Add(4*time.Second)); len(got) != 0 {
		t.Fatalf("repeated success: %v", got)
	}
	errors = 2
	check(d, now.Add(5*time.Second), "application errors: 2")
	d.Peers = 0
	check(d, now.Add(6*time.Second), "lost all mesh peers")
	check(d, now.Add(11*time.Minute), "1 valid message")
}
