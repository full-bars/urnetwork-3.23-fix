package urnettools

import "testing"

func TestClassicalSessions(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		clients, pqe, counted int
		want                  int
	}{
		{"most clients are not post-quantum", 1414, 3, 0, 1411},
		{"no post-quantum sessions", 551, 0, 0, 551},
		{"all clients post-quantum", 200, 200, 0, 0},
		{"the counted classical sessions are a floor", 212, 200, 20, 20},
		{"more post-quantum sessions than clients never goes negative", 5, 9, 0, 0},
		{"no clients", 0, 0, 0, 0},
	} {
		if got := classicalSessions(tc.clients, tc.pqe, tc.counted); got != tc.want {
			t.Errorf("%s: classicalSessions(%d, %d, %d) = %d, want %d", tc.name, tc.clients, tc.pqe, tc.counted, got, tc.want)
		}
	}
}
