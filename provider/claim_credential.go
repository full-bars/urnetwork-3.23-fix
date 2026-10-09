package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/docopt/docopt-go"
	"github.com/urfoundation/sn/ss58"
)

// claimCredential is the login `provider claim` authenticates with. The
// platform looks a claim up by the CLIENT in the token, so the normal claim
// needs a client token, not the network token. ByJwt is never printed.
type claimCredential struct {
	ByJwt string
	// LegacyColdkey is set only on the explicit network-token read of an epoch
	// that has no provider artifact.
	LegacyColdkey string
	// Source says where the token came from, for the operator. No secrets.
	Source string
}

// resolveClaimCredential picks exactly one credential source:
//
//	--store-client=<key>      a client token from ~/.urnetwork/.client_jwts.json
//	--provider-jwt=<path>     a client token in a file
//	--legacy-coldkey=<ss58>   the network token, plus an explicit coldkey
//
// With none given it refuses: a fleet node holds hundreds of client tokens and
// only a client that served traffic has a payout, so guessing one would
// silently claim nothing.
func resolveClaimCredential(opts docopt.Opts) (*claimCredential, error) {
	storeKey, _ := opts.String("--store-client")
	jwtPath, _ := opts.String("--provider-jwt")
	legacyColdkey, _ := opts.String("--legacy-coldkey")

	given := 0
	for _, v := range []string{storeKey, jwtPath, legacyColdkey} {
		if v != "" {
			given += 1
		}
	}
	if 1 < given {
		return nil, errors.New("pass only one of --store-client, --provider-jwt and --legacy-coldkey")
	}

	switch {
	case jwtPath != "":
		raw, err := os.ReadFile(jwtPath)
		if err != nil {
			return nil, fmt.Errorf("reading --provider-jwt file %s: %w", jwtPath, err)
		}
		return clientClaimCredential(strings.TrimSpace(string(raw)), "--provider-jwt file")

	case storeKey != "":
		path, err := providerStatePath(".client_jwts.json")
		if err != nil {
			return nil, err
		}
		store := newClientJWTStore(path)
		entry, ok := store.Get(storeKey)
		if !ok {
			return nil, fmt.Errorf("no client %q in %s (%d identities). The key is the proxy address the identity was minted for, or \"direct\"", storeKey, path, store.Count())
		}
		return clientClaimCredential(entry.ByClientJWT, "client token from the identity store")

	case legacyColdkey != "":
		if _, err := ss58.DecodeWithPrefix(legacyColdkey, ss58.BittensorPrefix); err != nil {
			return nil, fmt.Errorf("invalid --legacy-coldkey %q: %w", legacyColdkey, err)
		}
		byJwt, err := readNetworkJwt()
		if err != nil {
			return nil, err
		}
		if jwtContainsClientId(byJwt) {
			return nil, errors.New("the network jwt unexpectedly names a client; --legacy-coldkey needs the account (network) token")
		}
		return &claimCredential{ByJwt: byJwt, LegacyColdkey: legacyColdkey, Source: "network token with explicit legacy coldkey"}, nil
	}

	return nil, errors.New("claim needs the login of a client that served traffic: pass --store-client=<key> (an identity from the store), --provider-jwt=<file>, or --legacy-coldkey=<coldkey_ss58> for an epoch without a provider artifact")
}

// clientClaimCredential accepts a token only if it names a client and has not
// expired, so a network token or a stale one fails here and not as an opaque
// server refusal.
func clientClaimCredential(byJwt string, source string) (*claimCredential, error) {
	if byJwt == "" {
		return nil, fmt.Errorf("%s is empty", source)
	}
	if !jwtContainsClientId(byJwt) {
		return nil, fmt.Errorf("%s does not name a client (is it the network token?); claim needs a client token", source)
	}
	if err := validateJWTExpiry(byJwt); err != nil {
		return nil, fmt.Errorf("%s has expired; let the provider renew it or pass a fresher --provider-jwt", source)
	}
	return &claimCredential{ByJwt: byJwt, Source: source}, nil
}
