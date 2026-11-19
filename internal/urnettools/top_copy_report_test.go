package urnettools

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// A screen copy that could not be saved is a failed command: the text is still
// printed, and the error comes back so the exit status shows it.
func TestReportTopCopyReturnsTheCopyError(t *testing.T) {
	m := &topModel{copyText: "screen text", copyErr: errors.New("disk full")}
	var out, errOut bytes.Buffer
	err := reportTopCopy(m, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("a failed copy must return its error, got %v", err)
	}
	if !strings.Contains(out.String(), "screen text") || !strings.Contains(errOut.String(), "could not write the copy") {
		t.Fatalf("the text and the diagnostic are still printed: out=%q err=%q", out.String(), errOut.String())
	}
}

func TestReportTopCopyIsQuietOnSuccess(t *testing.T) {
	m := &topModel{copyText: "screen text", copyPath: "/state/top-1.txt"}
	var out, errOut bytes.Buffer
	if err := reportTopCopy(m, &out, &errOut); err != nil {
		t.Fatalf("a saved copy is no error: %v", err)
	}
	if !strings.Contains(errOut.String(), "copy written to /state/top-1.txt") {
		t.Fatalf("the path is reported: %q", errOut.String())
	}
}
