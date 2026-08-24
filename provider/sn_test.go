package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
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

// fixedEvmAddress builds a deterministic common.Address for use as the
// `registrant` argument in snSignBindHead tests.
func fixedEvmAddress(b byte) common.Address {
	var out common.Address
	for i := range out {
		out[i] = b
	}
	return out
}

func fixedBytes32(b byte) (out [32]byte) {
	for i := range out {
		out[i] = b
	}
	return out
}

func TestSnSignBindHead_FieldsCopiedAndSignatureVerifies(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %s", err)
	}

	registrant := fixedEvmAddress(0xAB)
	hotkey := fixedBytes32(0xCD)
	digest := fixedBytes32(0xEF)

	intent := snSignBindHead(priv, registrant, hotkey, digest)

	if intent.hotkey != hotkey {
		t.Errorf("intent.hotkey = %x; want %x", intent.hotkey, hotkey)
	}
	if intent.digest != digest {
		t.Errorf("intent.digest = %x; want %x", intent.digest, digest)
	}
	if intent.registrant != registrant {
		t.Errorf("intent.registrant = %x; want %x", intent.registrant, registrant)
	}
	if !bytes.Equal(intent.clientId[:], pub) {
		t.Errorf("intent.clientId = %x; want the ed25519 public key %x", intent.clientId, pub)
	}
	if len(intent.clientIdSig) != ed25519.SignatureSize {
		t.Fatalf("intent.clientIdSig length = %d; want %d", len(intent.clientIdSig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, digest[:], intent.clientIdSig) {
		t.Error("intent.clientIdSig does not verify against digest with the signing key's public key")
	}
}

func TestSnSignBindHead_SignatureIsBoundToDigest(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %s", err)
	}

	registrant := fixedEvmAddress(0x01)
	hotkey := fixedBytes32(0x02)
	digestA := fixedBytes32(0xAA)
	digestB := fixedBytes32(0xBB)

	intentA := snSignBindHead(priv, registrant, hotkey, digestA)
	intentB := snSignBindHead(priv, registrant, hotkey, digestB)

	if bytes.Equal(intentA.clientIdSig, intentB.clientIdSig) {
		t.Error("signatures over two different digests must differ")
	}

	// A signature minted for digestA must not verify against digestB: this
	// is the property bindHead relies on to prove the provider's identity
	// key actually signed *this* on-chain headBindDigest, not a replay of
	// a signature captured for a different one.
	pub := priv.Public().(ed25519.PublicKey)
	if ed25519.Verify(pub, digestB[:], intentA.clientIdSig) {
		t.Error("intentA's signature unexpectedly verifies against digestB")
	}
}

func TestSnSignBindHead_DeterministicForSameInputs(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %s", err)
	}

	registrant := fixedEvmAddress(0x03)
	hotkey := fixedBytes32(0x04)
	digest := fixedBytes32(0x05)

	intent1 := snSignBindHead(priv, registrant, hotkey, digest)
	intent2 := snSignBindHead(priv, registrant, hotkey, digest)

	// ed25519.Sign is deterministic (no per-call randomness): same key +
	// same message must reproduce the same signature every call.
	if !bytes.Equal(intent1.clientIdSig, intent2.clientIdSig) {
		t.Error("snSignBindHead produced different signatures for identical inputs")
	}
}
