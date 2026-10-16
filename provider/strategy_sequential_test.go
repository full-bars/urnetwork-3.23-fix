package main

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestApplyProviderStrategySequential(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", 0},
		{"off", 0},
		{" OFF ", 0},
		{"0", 0},
		{"on", strategySequentialDefaultHedge},
		{"true", strategySequentialDefaultHedge},
		{"1", strategySequentialDefaultHedge},
		{"400ms", 400 * time.Millisecond},
		{"garbage", 0},
		{"-5s", 0},
	}
	for _, c := range cases {
		t.Setenv("URNETWORK_STRATEGY_SEQUENTIAL", c.env)
		settings := connect.DefaultClientStrategySettings()
		applyProviderStrategySequential(settings)
		if settings.SequentialDialHedge != c.want {
			t.Fatalf("env %q: hedge = %s, want %s", c.env, settings.SequentialDialHedge, c.want)
		}
	}
}
