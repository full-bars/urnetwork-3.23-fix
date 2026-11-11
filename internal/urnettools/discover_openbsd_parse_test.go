package urnettools

import (
	"reflect"
	"testing"
)

func TestParseOpenBSDProcesses(t *testing.T) {
	input := " 123 1000 urnetwork\n456 0 provider\n0 0 provider\nno 0 provider\n34 -1 provider\n35 0 provider-tracker\n36 0 sh\n37 0 provider extra\n"
	want := []openBSDProcess{{pid: 123, uid: "1000"}, {pid: 456, uid: "0"}}
	if got := parseOpenBSDProcesses(input); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}
