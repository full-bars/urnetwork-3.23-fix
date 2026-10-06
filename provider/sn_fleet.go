package main

// sn_fleet.go — the fleet-bind/revoke machinery behind `provider bind-head`
// and `provider unbind-head` (sn/miner/fleet.go is the reference flow).
//
// The upstream sn line replaced the per-head bind/unbind model with the
// release-1.0 many-to-one dual-signed fleet binding: a FleetManifest names
// the fleet (chain, netuid, coordinator, fleet id, hotkey, generation,
// members), a binding carries one member's client_id/client_key, and BOTH
// the client key (Ed25519) and the hotkey (sr25519) sign the same
// domain-separated digest. Revocation ends one binding generation at a
// future epoch, signed by the client. These commands keep their names and
// their offline-print/EVM-submit shape; the flags follow the new model
// (manifest + hotkey seed + epochs).

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"

	"github.com/docopt/docopt-go"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/vedhavyas/go-subkey/v2/sr25519"

	"github.com/urfoundation/sn/miner/onchain"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/stabi"
)

// stCoordinator holds the shared abigen packers/unpackers for STCoordinator
// (fleet bind/revoke), mirroring stSubnet for the subnet contract.
var stCoordinator = stabi.NewSTCoordinator()

// parseClientID16Arg parses a 16-byte client id (0x-optional hex, as the
// manifest members use).
func parseClientID16Arg(field string, value string) ([16]byte, error) {
	var out [16]byte
	b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(value), "0x"), "0X"))
	if err != nil || len(b) != len(out) {
		return out, fmt.Errorf("%s must be a 16-byte hex value", field)
	}
	copy(out[:], b)
	return out, nil
}

// providerClientId16 reads the provider's own client_id from the client JWT
// store ("direct" = the native connection's identity).
func providerClientId16() ([16]byte, error) {
	var out [16]byte
	store := globalClientJWTStore
	store.mu.Lock()
	store.loadLocked()
	entry, ok := store.entries["direct"]
	store.mu.Unlock()
	if !ok || strings.TrimSpace(entry.ClientID) == "" {
		return out, fmt.Errorf("no provider client_id in the client JWT store; pass --client_id=<hex16>")
	}
	compact := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(entry.ClientID)), "-", "")
	b, err := hex.DecodeString(compact)
	if err != nil || len(b) != len(out) {
		return out, fmt.Errorf("provider client_id %q is not a 16-byte uuid", entry.ClientID)
	}
	copy(out[:], b)
	return out, nil
}

// snLoadFleetManifestOpt loads and validates the --manifest file.
func snLoadFleetManifestOpt(opts docopt.Opts) (*protocol.FleetManifest, error) {
	path, _ := opts.String("--manifest")
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("--manifest: the fleet manifest is required (the fleet model is manifest-driven)")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--manifest: %s", err)
	}
	manifest, err := protocol.ParseFleetManifest(raw)
	if err != nil {
		return nil, fmt.Errorf("--manifest: %s", err)
	}
	return manifest, nil
}

// snClientIdOpt resolves the client id: --client_id, else the provider's own.
func snClientIdOpt(opts docopt.Opts) ([16]byte, error) {
	value, _ := opts.String("--client_id")
	if strings.TrimSpace(value) != "" {
		return parseClientID16Arg("--client_id", value)
	}
	return providerClientId16()
}

// snUint64Opt parses a required decimal uint64 option.
func snUint64Opt(opts docopt.Opts, name string) (uint64, error) {
	value, _ := opts.String(name)
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %s (required)", name, err)
	}
	return parsed, nil
}

// snClientKeyOpt resolves the client signing key: --client_seed_file, else
// the provider's own client identity key.
func snClientKeyOpt(opts docopt.Opts) (ed25519.PrivateKey, error) {
	seedFile, _ := opts.String("--client_seed_file")
	if strings.TrimSpace(seedFile) != "" {
		return snLoadClientSeedOverride(seedFile)
	}
	return snLoadClientKey()
}

// snFleetMember finds the manifest member for a client id.
func snFleetMember(manifest *protocol.FleetManifest, clientID [16]byte) (protocol.FleetMember, error) {
	for _, member := range manifest.Members {
		if member.ClientID == clientID {
			return member, nil
		}
	}
	return protocol.FleetMember{}, fmt.Errorf("client_id 0x%x is not in the manifest", clientID)
}

// snLoadHotkeySeed loads an sr25519 hotkey seed file (raw 32 bytes, or hex
// text, mirroring how sn/miner loads seeds).
func snLoadHotkeySeed(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) != 32 {
		trimmed := strings.TrimSpace(string(raw))
		decoded, decodeErr := hex.DecodeString(strings.TrimPrefix(trimmed, "0x"))
		if decodeErr != nil || len(decoded) != 32 {
			return nil, fmt.Errorf("%s: expected raw or hex 32-byte sr25519 seed", path)
		}
		raw = decoded
	}
	return raw, nil
}

// snLoadClientSeedOverride loads an Ed25519 client seed file (raw 32 bytes,
// or hex text).
func snLoadClientSeedOverride(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.SeedSize {
		trimmed := strings.TrimSpace(string(raw))
		decoded, decodeErr := hex.DecodeString(strings.TrimPrefix(trimmed, "0x"))
		if decodeErr != nil || len(decoded) != ed25519.SeedSize {
			return nil, fmt.Errorf("%s: expected raw or hex 32-byte Ed25519 seed", path)
		}
		raw = decoded
	}
	return ed25519.NewKeyFromSeed(raw), nil
}

// snFleetBindingAndSign mirrors sn/miner's fleetBindingAndSign: build the
// binding from the manifest + member + epochs, then dual-sign it — client
// Ed25519 over the digest, hotkey sr25519 over the same digest.
func snFleetBindingAndSign(manifest *protocol.FleetManifest, member protocol.FleetMember, from, to uint64, clientKey ed25519.PrivateKey, hotkeySeed []byte) (protocol.FleetBinding, []byte, []byte, error) {
	binding, err := manifest.Binding(member, from, to)
	if err != nil {
		return protocol.FleetBinding{}, nil, nil, err
	}
	clientSignature, err := binding.SignClient(clientKey)
	if err != nil {
		return binding, nil, nil, err
	}
	hotkey, err := (sr25519.Scheme{}).FromSeed(hotkeySeed)
	if err != nil {
		return binding, nil, nil, err
	}
	if !bytesEqualConst(hotkey.Public(), manifest.Hotkey[:]) {
		return binding, nil, nil, errors.New("hotkey seed does not match the manifest hotkey")
	}
	digest, err := binding.Digest()
	if err != nil {
		return binding, nil, nil, err
	}
	hotkeySignature, err := hotkey.Sign(digest[:])
	if err != nil {
		return binding, nil, nil, err
	}
	return binding, clientSignature, hotkeySignature, nil
}

func bytesEqualConst(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	diff := byte(0)
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// snReadFleetRevokeDigest reads the coordinator's canonical revoke digest
// via eth_call (finalized), trying each endpoint in order — the same
// read-side transport as the claim path (sn_rpc.go), against the
// coordinator contract.
func snReadFleetRevokeDigest(ctx context.Context, rpcUrls []string, coordinatorHex string, calldata []byte) (digest [32]byte, rpcUrl string, err error) {
	for _, url := range rpcUrls {
		callHex, rpcErr := ethRpcHexResult(ctx, url, "eth_call", []any{
			map[string]any{
				"to":   coordinatorHex,
				"data": fmt.Sprintf("0x%x", calldata),
			},
			"finalized",
		})
		if rpcErr != nil {
			fmt.Printf("rpc %s: %s\n", url, rpcErr)
			continue
		}
		returnData, rpcErr := parseEthHexBytes(callHex)
		if rpcErr != nil || len(returnData) < 32 {
			fmt.Printf("rpc %s: fleet revoke digest returned %d bytes; expected >= 32 (wrong coordinator address?)\n", url, len(returnData))
			continue
		}
		copy(digest[:], returnData[:32])
		return digest, url, nil
	}
	return digest, "", fmt.Errorf("no --rpc endpoint answered the fleet revoke digest")
}

// snFleetSubmit submits coordinator calldata through sn/miner/onchain as
// the relayer (the same path the claim command uses).
func snFleetSubmit(ctx context.Context, manifest *protocol.FleetManifest, rpcUrls []string, keyFile string, calldata []byte, dryRun bool) (*types.Receipt, error) {
	key, err := onchain.LoadKeyFile(keyFile)
	if err != nil {
		return nil, err
	}
	return onchain.Submit(ctx, onchain.SubmitParams{
		Contract: common.Address(manifest.Coordinator),
		Rpcs:     rpcUrls,
		Key:      key,
		Calldata: calldata,
		ChainID:  new(big.Int).SetUint64(manifest.ChainID),
		DryRun:   dryRun,
	})
}
