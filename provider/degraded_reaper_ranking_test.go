package main

import "testing"

// An unreachable upstream freezes every proxy's contribution at the same value,
// so the ordering ranks nothing. Reaping on it is what cut a healthy pool in half
// during the 2026-10-23 platform outage, when every proxy read zero at once.
func TestScoresCarryNoRanking(t *testing.T) {
	tied := []scoredDegradedProxy{{score: 0}, {score: 0}, {score: 0}}
	if !scoresCarryNoRanking(tied) {
		t.Fatal("identical scores must not rank anything")
	}
	differing := []scoredDegradedProxy{{score: 0}, {score: 4096}, {score: 0}}
	if scoresCarryNoRanking(differing) {
		t.Fatal("a differing score is a real ranking")
	}
	if !scoresCarryNoRanking([]scoredDegradedProxy{{score: 7}}) {
		t.Fatal("a single candidate must not be reaped on a ranking")
	}
	if !scoresCarryNoRanking(nil) {
		t.Fatal("an empty set must not be reaped on a ranking")
	}
}
