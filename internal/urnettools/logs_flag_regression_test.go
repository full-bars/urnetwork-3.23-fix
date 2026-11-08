package urnettools

import (
	"strings"
	"testing"
)

func TestLogsRejectsImportantFlag(t *testing.T) {
	_, _, err := parseTargetFlags([]string{"--important"})
	if err == nil {
		t.Fatal("expected error for unknown flag --important, got nil")
	}
	if !strings.Contains(err.Error(), "--important") {
		t.Fatalf("expected error mentioning --important, got: %v", err)
	}
}
