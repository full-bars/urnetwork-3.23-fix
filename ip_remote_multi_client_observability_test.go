package connect

import (
	"errors"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
)

func TestRelEventGrammar(t *testing.T) {
	id, _ := ParseId("018e3a5e-1111-7222-8333-444455556666")
	assert.Equal(t, relEvent("x"), "[rel] event=x")
	assert.Equal(t,
		relEvent("sticky_redial", "Provider", id, "retry", 1500*time.Millisecond, "ok", true, "n", 3),
		"[rel] event=sticky_redial provider=55556666 retry=1500 ok=1 n=3",
	)
	assert.Equal(t,
		relEvent("e", "err", errors.New("a b"), "empty", "", "nil", nil, "dangling"),
		`[rel] event=e err="a b" empty="" nil=-`,
	)
}
