package urnettools

import (
	"reflect"
	"testing"
)

func TestExtenderSetupElevationKeepsExplicitConfirmation(t *testing.T) {
	old := runExtenderSetupFn
	defer func() { runExtenderSetupFn = old }()
	for _, args := range [][]string{{"setup"}, {"setup", "--yes"}} {
		var got ExtenderSetupOptions
		runExtenderSetupFn = func(opts ExtenderSetupOptions) error { got = opts; return nil }
		if err := cmdExtenderSetup(args); err != nil {
			t.Fatal(err)
		}
		want := append([]string{"extender"}, args...)
		if !reflect.DeepEqual(got.Argv, want) {
			t.Fatalf("argv = %q, want %q", got.Argv, want)
		}
	}
}
