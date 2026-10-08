package main

import (
	"strings"
	"testing"
)

func TestParseBytes32Arg(t *testing.T) {
	valid32 := "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	var wantValid [32]byte
	for i := range wantValid {
		wantValid[i] = byte(i + 1)
	}

	tests := []struct {
		name    string
		field   string
		input   string
		want    [32]byte
		wantErr bool
	}{
		{
			name:  "valid hex without 0x prefix",
			field: "--hotkey",
			input: valid32,
			want:  wantValid,
		},
		{
			name:  "valid hex with 0x prefix",
			field: "--hotkey",
			input: "0x" + valid32,
			want:  wantValid,
		},
		{
			name:  "valid hex with 0X prefix (uppercase)",
			field: "--hotkey",
			input: "0X" + valid32,
			want:  wantValid,
		},
		{
			name:  "valid hex with surrounding whitespace",
			field: "--hotkey",
			input: "  0x" + valid32 + "  ",
			want:  wantValid,
		},
		{
			name:  "uppercase hex digits",
			field: "--hotkey",
			input: "0x" + strings.ToUpper(valid32),
			want:  wantValid,
		},
		{
			name:    "too short",
			field:   "--hotkey",
			input:   "0x" + valid32[:62],
			wantErr: true,
		},
		{
			name:    "too long",
			field:   "--hotkey",
			input:   "0x" + valid32 + "ff",
			wantErr: true,
		},
		{
			name:    "odd number of hex digits",
			field:   "--hotkey",
			input:   "0x" + valid32[:63],
			wantErr: true,
		},
		{
			name:    "non-hex characters",
			field:   "--hotkey",
			input:   "0x" + strings.Repeat("zz", 32),
			wantErr: true,
		},
		{
			name:    "empty string",
			field:   "--hotkey",
			input:   "",
			wantErr: true,
		},
		{
			name:    "bare 0x with nothing else",
			field:   "--hotkey",
			input:   "0x",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBytes32Arg(tt.field, tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseBytes32Arg(%q, %q) = %x, nil; want error", tt.field, tt.input, got)
				}
				if !strings.Contains(err.Error(), tt.field) {
					t.Errorf("parseBytes32Arg error %q does not mention field %q", err.Error(), tt.field)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBytes32Arg(%q, %q) unexpected error: %s", tt.field, tt.input, err)
			}
			if got != tt.want {
				t.Errorf("parseBytes32Arg(%q, %q) = %x; want %x", tt.field, tt.input, got, tt.want)
			}
		})
	}
}

func TestParseEvmAddressArg(t *testing.T) {
	valid20 := "0102030405060708090a0b0c0d0e0f1011121314"
	var wantValid [20]byte
	for i := range wantValid {
		wantValid[i] = byte(i + 1)
	}

	tests := []struct {
		name    string
		field   string
		input   string
		want    [20]byte
		wantErr bool
	}{
		{
			name:  "valid address without 0x prefix",
			field: "--registrant",
			input: valid20,
			want:  wantValid,
		},
		{
			name:  "valid address with 0x prefix",
			field: "--registrant",
			input: "0x" + valid20,
			want:  wantValid,
		},
		{
			name:  "valid address with whitespace",
			field: "--registrant",
			input: " 0x" + valid20 + "\n",
			want:  wantValid,
		},
		{
			name:    "too short (looks like a bytes32 truncated)",
			field:   "--registrant",
			input:   "0x" + valid20[:38],
			wantErr: true,
		},
		{
			name:    "too long (32-byte value passed where 20 expected)",
			field:   "--registrant",
			input:   "0x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
			wantErr: true,
		},
		{
			name:    "non-hex characters",
			field:   "--registrant",
			input:   "0x" + strings.Repeat("gg", 20),
			wantErr: true,
		},
		{
			name:    "odd number of hex digits",
			field:   "--registrant",
			input:   "0x" + valid20[:39],
			wantErr: true,
		},
		{
			name:    "bare 0x with nothing else",
			field:   "--registrant",
			input:   "0x",
			wantErr: true,
		},
		{
			name:    "empty string",
			field:   "--registrant",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseEvmAddressArg(tt.field, tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseEvmAddressArg(%q, %q) = %x, nil; want error", tt.field, tt.input, got)
				}
				if !strings.Contains(err.Error(), tt.field) {
					t.Errorf("parseEvmAddressArg error %q does not mention field %q", err.Error(), tt.field)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseEvmAddressArg(%q, %q) unexpected error: %s", tt.field, tt.input, err)
			}
			if got != tt.want {
				t.Errorf("parseEvmAddressArg(%q, %q) = %x; want %x", tt.field, tt.input, got, tt.want)
			}
		})
	}
}

func TestParseEvmAddressArg_UppercasePrefix(t *testing.T) {
	valid20 := "0102030405060708090a0b0c0d0e0f1011121314"
	var want [20]byte
	for i := range want {
		want[i] = byte(i + 1)
	}

	// parseBytes32Arg's "0X" (uppercase prefix) case is covered above;
	// parseEvmAddressArg shares the same TrimPrefix("0x")/TrimPrefix("0X")
	// logic and deserves the same regression coverage on its own, since a
	// future refactor could accidentally decouple the two implementations.
	got, err := parseEvmAddressArg("--registrant", "0X"+valid20)
	if err != nil {
		t.Fatalf("parseEvmAddressArg(%q) unexpected error: %s", "0X"+valid20, err)
	}
	if got != want {
		t.Errorf("parseEvmAddressArg(%q) = %x; want %x", "0X"+valid20, got, want)
	}
}

func fixedBytes32(b byte) (out [32]byte) {
	for i := range out {
		out[i] = b
	}
	return out
}
