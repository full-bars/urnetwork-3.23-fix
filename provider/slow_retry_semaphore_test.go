package main

import "testing"

// The ceiling main.go's provide loop uses; only its relation to authFailures matters here.
const testMaxAuthFailures = 10

// The slow-retry semaphore caps how many failing paid or file proxies can sit
// in the auth pipeline at once. A genuine give-up pins authFailures one BELOW
// the ceiling and zeroes slowRetryCycles, so the condition must also look at
// the genuine retry cycle counter or every ramp and daily attempt after the
// first give-up skips the semaphore and the whole failing set hits the auth API
// together.
func TestUsesSlowRetrySemaphore(t *testing.T) {
	cases := []struct {
		name                     string
		proxy, urlSourced        bool
		authFailures             int
		slowCycles, genuineCycle int
		want                     bool
	}{
		{"fresh proxy in its first ladder", true, false, 0, 0, 0, false},
		{"ladder exhausted", true, false, testMaxAuthFailures, 0, 0, true},
		{"in the slow retry ramp", true, false, testMaxAuthFailures - 1, 1, 0, true},
		{"after a genuine give-up (authFailures pinned below the ceiling)", true, false, testMaxAuthFailures - 1, 0, 1, true},
		{"later genuine cycle", true, false, testMaxAuthFailures - 1, 0, 7, true},
		{"URL-sourced proxies never use it", true, true, testMaxAuthFailures, 2, 3, false},
		{"the direct connection never queues", false, false, testMaxAuthFailures, 2, 3, false},
	}
	for _, tc := range cases {
		got := usesSlowRetrySemaphore(tc.proxy, tc.urlSourced, tc.authFailures, testMaxAuthFailures, tc.slowCycles, tc.genuineCycle)
		if got != tc.want {
			t.Errorf("%s: usesSlowRetrySemaphore = %v, want %v", tc.name, got, tc.want)
		}
	}
}
