package urnettools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"
)

// This file restores `urnet-tools session save|load`, which the Go rewrite
// dropped. The legacy shell tool (Provider_Install_Linux.sh do_session)
// exported and imported the provider's identity + proxy state as an encrypted
// bundle. This port is pure Go (stdlib crypto) so it works on Windows, macOS,
// and Linux without an external openssl binary.
//
// Bundle format:
//   v1 (legacy): "Salted__" + 8-byte salt + AES-256-CBC (PBKDF2-HMAC-SHA256,
//                10000 iters) ciphertext — matches `openssl enc -aes-256-cbc
//                -pbkdf2`. Only authenticated by gzip CRC + PKCS#7 padding;
//                malleable. Still readable on load for backward compatibility.
//   v2 (current): "URNSv2\0\0" + 16-byte salt + 12-byte nonce + AES-256-GCM
//                ciphertext (PBKDF2-HMAC-SHA256, 600000 iters) + 16-byte tag.
//                Authenticated, with an iteration count that meets current
//                brute-force guidance.

// sessionFiles are the state-dir files that make up an identity session
// bundle. Mirror of the legacy do_session collection list.
var sessionFiles = []string{
	".client_jwts.json",
	"jwt",
	"jwt_last_refresh",
	".provider.key",
	".provider.cert",
	"proxy",
	"proxy_url.json",
	"proxy.state",
}

// tarAndEncrypt builds the session bundle from an ordered name->bytes map. It
// gzips a tar of the entries, then AES-256-GCM encrypts it with a key derived
// by PBKDF2-HMAC-SHA256 at sessionPBKDF2Iters (current guidance: 600000)
// from a user passphrase, returning the v2 blob. The header is
// "URNSv2\0\0" + salt + nonce + AES-256-GCM ciphertext (which already
// includes the 16-byte tag at the tail).
func tarAndEncrypt(files map[string][]byte, pass string) ([]byte, error) {
	pt, err := buildSessionInnerTar(files)
	if err != nil {
		return nil, err
	}
	salt, err := sessionRand(16)
	if err != nil {
		return nil, err
	}
	key, err := deriveKey(pass, salt, sessionPBKDF2Iters, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	nonce, err := sessionRand(12)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ct := aead.Seal(nil, nonce, pt, sessionBundleV2Header)
	out := make([]byte, 0, len(sessionBundleV2Header)+len(salt)+len(nonce)+len(ct))
	out = append(out, sessionBundleV2Header...)
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// deriveKey derives a key of the requested length from the passphrase and
// salt via PBKDF2-HMAC-SHA256. iters must meet current brute-force guidance
// (>= 600000) for new bundles; legacy load paths use a small count for the
// v1 round-trip test only.
func deriveKey(pass string, salt []byte, iters, keyLen int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, pass, salt, iters, keyLen)
}

// sessionPBKDF2Iters is the iteration count used for new v2 bundles.
// 600000 matches the 2023+ PBKDF2-HMAC-SHA256 guidance (OWASP). The
// iteration cost is paid once at save/load time on a single CPU — the
// security property it buys is offline brute-force resistance on a stolen
// bundle, where the attacker has all the time they want.
const sessionPBKDF2Iters = 600000

// sessionLegacyPBKDF2Iters is used only to verify legacy v1 openssl-style
// bundles. The legacy scheme was deliberately weak; we do not lower our
// new-bundle cost to match.
const sessionLegacyPBKDF2Iters = 10000

// sessionBundleV2Header marks the start of a v2 bundle and is also the
// associated-data input to AES-GCM, so a v1 bundle cannot be reinterpreted
// as v2 by an attacker.
var sessionBundleV2Header = []byte("URNSv2\x00\x00")

// buildSessionInnerTar produces the inner gzip-compressed tar of
// sessionFiles from files (only entries present in files are included,
// matching legacy).
func buildSessionInnerTar(files map[string][]byte) ([]byte, error) {
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	for _, name := range sessionFiles {
		data, ok := files[name]
		if !ok {
			continue
		}
		hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Unix(0, 0)}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return raw.Bytes(), nil
}

// sessionRand returns n random bytes. Package var so tests can pin output.
var sessionRand = func(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// decryptUntar reverses tarAndEncrypt: it auto-detects the bundle version,
// derives the key, decrypts, and expands the gzip tar into a name->bytes
// map. v1 (legacy openssl "Salted__" + AES-256-CBC) is still accepted for
// backward compatibility; v2 is the AES-256-GCM authenticated format.
// Returns an error on a wrong passphrase or corrupt bundle — GCM makes a
// wrong-pass failure a single, deterministic rejection rather than the
// ambiguous "bad padding OR wrong key" that CBC allowed.
func decryptUntar(bundle, pass string) (map[string][]byte, error) {
	raw := []byte(bundle)
	if len(raw) < len(sessionBundleV2Header) {
		return nil, errors.New("not a session bundle (too short)")
	}
	switch {
	case string(raw[:len(sessionBundleV2Header)]) == string(sessionBundleV2Header):
		return decryptUntarV2(raw, pass)
	case string(raw[:8]) == "Salted__":
		return decryptUntarV1(raw, pass)
	default:
		return nil, errors.New("not a recognized session bundle header")
	}
}

// decryptUntarV2 parses the v2 (AES-256-GCM, PBKDF2 600000) format and
// returns the decrypted file map. v2 is authenticated: any tampering or
// wrong passphrase fails GCM Open with a uniform error.
func decryptUntarV2(raw []byte, pass string) (map[string][]byte, error) {
	const headerLen = len("URNSv2\x00\x00") // 8
	const saltLen = 16
	const nonceLen = 12
	const tagLen = 16
	minLen := headerLen + saltLen + nonceLen + tagLen
	if len(raw) < minLen {
		return nil, errors.New("corrupt v2 bundle: too short")
	}
	salt := raw[headerLen : headerLen+saltLen]
	nonce := raw[headerLen+saltLen : headerLen+saltLen+nonceLen]
	ct := raw[headerLen+saltLen+nonceLen:]
	key, err := deriveKey(pass, salt, sessionPBKDF2Iters, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	pt, err := aead.Open(nil, nonce, ct, sessionBundleV2Header)
	if err != nil {
		return nil, errors.New("invalid passphrase or corrupt bundle (authentication failed)")
	}
	return untarGz(pt)
}

// decryptUntarV1 parses the legacy openssl "Salted__" + AES-256-CBC bundle
// (PBKDF2 10000). Still accepted so old bundles migrate forward — the
// operator runs session save again to upgrade. The output is NOT
// authenticated; callers should re-encrypt promptly.
func decryptUntarV1(raw []byte, pass string) (map[string][]byte, error) {
	if len(raw) < 16 || string(raw[:8]) != "Salted__" {
		return nil, errors.New("not an openssl 'Salted__' session bundle")
	}
	salt := raw[8:16]
	ct := raw[16:]
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, errors.New("corrupt v1 bundle: bad ciphertext length")
	}
	key, err := deriveKey(pass, salt, sessionLegacyPBKDF2Iters, 48)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return nil, err
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, key[32:48]).CryptBlocks(pt, ct)
	if len(pt) == 0 {
		return nil, errors.New("corrupt v1 bundle: empty plaintext")
	}
	pad := int(pt[len(pt)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(pt) {
		return nil, errors.New("invalid passphrase or corrupt bundle (bad padding)")
	}
	for _, b := range pt[len(pt)-pad:] {
		if int(b) != pad {
			return nil, errors.New("invalid passphrase or corrupt bundle (bad padding)")
		}
	}
	pt = pt[:len(pt)-pad]
	return untarGz(pt)
}

// untarGz decompresses the inner gzip tar and returns a name->bytes map.
// It bounds the entry size via io.LimitReader so a crafted bundle cannot
// OOM the tool on read.
func untarGz(pt []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(pt))
	if err != nil {
		return nil, errors.New("invalid passphrase or corrupt bundle (bad archive)")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("invalid passphrase or corrupt bundle (bad archive)")
		}
		// Cap each entry's size — a corrupted bundle with a misleading
		// Content-Length-equivalent in the tar header cannot exhaust
		// memory. 64 MiB is well above any session file's real size.
		const maxEntry = 64 << 20
		lr := io.LimitReader(tr, maxEntry)
		data, err := io.ReadAll(lr)
		if err != nil {
			return nil, err
		}
		files[hdr.Name] = data
	}
	return files, nil
}

// collectSessionFiles reads the identity files that exist under a state dir
// into a name->bytes map (only present files are included, matching legacy).
func collectSessionFiles(stateDir string) map[string][]byte {
	out := map[string][]byte{}
	for _, name := range sessionFiles {
		b, err := os.ReadFile(filepath.Join(stateDir, name))
		if err != nil {
			continue
		}
		out[name] = b
	}
	return out
}

// sessionHasJWT reports whether a decrypted bundle carries a jwt, the
// identity that load requires before it will stage anything.
func sessionHasJWT(files map[string][]byte) bool {
	_, ok := files["jwt"]
	return ok
}

// sessionNetworkID extracts the network_id from a bundled jwt ("" if absent
// or invalid). Used to enforce the same-account rule on load.
func sessionNetworkID(files map[string][]byte) string {
	raw, ok := files["jwt"]
	if !ok {
		return ""
	}
	_, netID, _, err := decodeJWTFromBytes(raw)
	if err != nil {
		return ""
	}
	return netID
}

// decodeJWTFromBytes decodes a JWT from a byte slice. Files land as bytes
// after an untar, whereas the codebase's decodeJWT takes a path; this is the
// in-memory variant used to read a staged jwt before any file is written.
// It reuses the same jwtPayload claim struct the codebase already defines.
func decodeJWTFromBytes(raw []byte) (netName, netID string, exp time.Time, err error) {
	parts := strings.Split(strings.TrimSpace(string(raw)), ".")
	if len(parts) != 3 {
		return "", "", time.Time{}, fmt.Errorf("not a JWT (%d segments)", len(parts))
	}
	payload := parts[1]
	if m := len(payload) % 4; m != 0 {
		payload += strings.Repeat("=", 4-m)
	}
	dec, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("payload decode: %w", err)
	}
	var p jwtPayload
	if err := json.Unmarshal(dec, &p); err != nil {
		return "", "", time.Time{}, fmt.Errorf("payload json: %w", err)
	}
	if p.Exp > 0 {
		exp = time.Unix(p.Exp, 0)
	}
	return p.NetworkName, p.NetworkID, exp, nil
}

// readPassphrase prints prompt and reads a passphrase with echo off (x/term).
// It requires an interactive terminal: a passphrase must never be scripted from
// piped stdin, and a second bufio.Reader must not be opened over os.Stdin (the
// package owns the single stdinReader). Refuse when not interactive rather than
// block or read a buffered leftover.
func readPassphrase(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	if !stdinIsInteractive() {
		return "", fmt.Errorf("stdin is not a terminal; passphrase entry requires an interactive session")
	}
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// readYesNo prompts for a y/N answer and returns true only on an explicit yes.
// Uses the shared confirmStdinRead so it honors the single-reader rule and
// refuses cleanly on non-interactive stdin.
func readYesNo(prompt string) bool {
	line, err := confirmStdinRead(prompt)
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// cmdSession dispatches `urnet-tools session save|load <file> [target]`.
// It is wired WITHOUT parseGlobalFlags (like auth) so a provider --force/-f on
// load reaches this command instead of being swallowed by the tool's global
// parser.
func cmdSession(args []string) error {
	if hasHelpFlag(args) {
		fmt.Fprint(os.Stderr, `urnet-tools session — export/import provider identity + proxy state

Usage: urnet-tools session save <file> [target]
       urnet-tools session load <file> [target] [-f] [-n] [--allow-different-account]

save bundles the provider's identity and proxy state into an encrypted file.
load restores a bundle, backing up the current state first, and prompts to
restart. The loaded session must match the same URnetwork account unless
--allow-different-account is given. -f skips the confirmation prompt; -n prints
the plan and changes nothing.

Examples:
  urnet-tools session save ~/urnet-session.enc               # Linux / macOS
  urnet-tools session save C:\Users\<you>\urnet-session.enc   # Windows (\ or / separators)
  urnet-tools session load ~/urnet-session.enc --unit urnetwork-native.service
`)
		return nil
	}
	if len(args) == 0 {
		return errors.New("session requires an action: save <file> | load <file>")
	}
	action := args[0]
	force, dryRun, allowDiff := false, false, false
	var rest []string
	for _, a := range args[1:] {
		switch a {
		case "-f", "--force":
			force = true
		case "-n", "--dry-run":
			dryRun = true
		case "--allow-different-account":
			allowDiff = true
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) < 1 {
		return fmt.Errorf("session %s requires a file path", action)
	}
	file := rest[0]
	t, targetRest, err := parseTargetFlags(rest[1:])
	if err != nil {
		return err
	}
	if len(targetRest) > 0 {
		return fmt.Errorf("session takes no extra arguments (got %v)", targetRest)
	}
	p, err := selectTarget(lifecycleCandidates(t), t)
	if err != nil {
		return err
	}
	if err := guardSystemdProvider(p); err != nil {
		return err
	}
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	switch action {
	case "save":
		return cmdSessionSave(p, file)
	case "load":
		return cmdSessionLoad(p, file, force, dryRun, allowDiff)
	default:
		return fmt.Errorf("session action must be 'save' or 'load' (got %q)", action)
	}
}

// cmdSessionSave encrypts the provider's identity files into outFile.
func cmdSessionSave(p Provider, outFile string) error {
	fmt.Fprintln(os.Stderr, "WARNING: this bundle contains full identity and reputation credentials for this provider. Treat it like a password.")
	pass, err := readPassphrase("Enter encryption passphrase (will not echo): ")
	if err != nil {
		return err
	}
	confirm, err := readPassphrase("Confirm passphrase: ")
	if err != nil {
		return err
	}
	if pass != confirm {
		return errors.New("passphrases do not match")
	}
	if strings.TrimSpace(pass) == "" {
		return errors.New("passphrase cannot be empty")
	}
	files := collectSessionFiles(p.StateDir)
	if len(files) == 0 {
		return fmt.Errorf("no session files found under %s", p.StateDir)
	}
	// Refuse to clobber an existing destination silently (running as root,
	// os.WriteFile truncates whatever path is named). Require an explicit yes
	// to overwrite.
	if _, err := os.Stat(outFile); err == nil {
		if !readYesNo("destination already exists — overwrite? (y/n):") {
			return fmt.Errorf("aborted; %s already exists", outFile)
		}
	}
	bundle, err := tarAndEncrypt(files, pass)
	if err != nil {
		return fmt.Errorf("failed to create session bundle: %v", err)
	}
	f, err := os.OpenFile(outFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("write %s: %v", outFile, err)
	}
	if _, err := f.Write(bundle); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %v", outFile, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %v", outFile, err)
	}
	if err := os.Chmod(outFile, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %v", outFile, err)
	}
	fmt.Printf("Session saved to %s (encrypted)\n", outFile)
	return nil
}

// stageSessionFiles validates the same-account rule, backs up the live state,
// stages the new files, and marks the load pending. Extracted from
// cmdSessionLoad so the load safety logic is directly unit-testable with a
// temp state dir and in-memory files (no live provider, no interactive prompt).
func stageSessionFiles(p Provider, files map[string][]byte, allowDiff bool) (string, error) {
	newID := sessionNetworkID(files)
	if newID == "" {
		return "", errors.New("could not extract network_id from the session bundle's JWT; bundle may be corrupt")
	}
	// Read the current identity. Distinguish "no jwt" (fresh provider, no
	// account to enforce against) from "jwt present but unreadable", which
	// must fail closed rather than silently skip the same-account gate
	//: a truncated/corrupt live jwt must not let a bundle
	// for a different network load as if no identity existed.
	currentID := ""
	jwtPath := filepath.Join(p.StateDir, "jwt")
	if b, err := os.ReadFile(jwtPath); err == nil {
		_, cur, _, decErr := decodeJWTFromBytes(b)
		if decErr != nil {
			if allowDiff {
				currentID = "" // operator explicitly replaced the corrupt identity
			} else {
				return "", fmt.Errorf("current provider jwt at %s is present but unreadable: %v (use --allow-different-account to replace)", jwtPath, decErr)
			}
		} else {
			currentID = cur
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("could not read current provider jwt at %s: %v", jwtPath, err)
	}
	if currentID != "" && newID != currentID && !allowDiff {
		return "", fmt.Errorf("network ID mismatch (current=%s, session=%s); a session loads only under the same account (use --allow-different-account)", currentID, newID)
	}

	// Back up the live state before touching anything. Nanosecond suffix avoids
	// two rapid loads colliding over one backup dir.
	backupDir := filepath.Join(p.StateDir, ".session-backup-"+time.Now().Format("20060102-150405.000000000"))
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", err
	}
	if err := chownLikeStateOwner(p.StateDir, backupDir); err != nil {
		return "", err
	}
	for _, name := range sessionFiles {
		b, err := os.ReadFile(filepath.Join(p.StateDir, name))
		if err != nil {
			if os.IsNotExist(err) {
				continue // absent, fine
			}
			return "", fmt.Errorf("backup %s: %v", name, err) // unreadable/perm: fail, do not silently skip (MEDIUM)
		}
		if err := os.WriteFile(filepath.Join(backupDir, name), b, 0o600); err != nil {
			return "", fmt.Errorf("backup %s: %v", name, err)
		}
		if err := chownLikeStateOwner(p.StateDir, filepath.Join(backupDir, name)); err != nil {
			return "", err
		}
	}

	// Stage the new files; the provider applies .session-staging on its next
	// start and is told a load is pending via .session-pending.
	stagingDir := filepath.Join(p.StateDir, ".session-staging")
	if err := os.RemoveAll(stagingDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return "", err
	}
	if err := chownLikeStateOwner(p.StateDir, stagingDir); err != nil {
		return "", err
	}
	for _, name := range sessionFiles {
		if data, ok := files[name]; ok {
			if err := os.WriteFile(filepath.Join(stagingDir, name), data, 0o600); err != nil {
				return "", err
			}
			if err := chownLikeStateOwner(p.StateDir, filepath.Join(stagingDir, name)); err != nil {
				return "", err
			}
		}
	}
	pending := filepath.Join(p.StateDir, ".session-pending")
	if err := os.WriteFile(pending, []byte{}, 0o600); err != nil {
		return "", err
	}
	if err := chownLikeStateOwner(p.StateDir, pending); err != nil {
		return "", err
	}
	return backupDir, nil
}

// cmdSessionLoad decrypts a session bundle and stages it into the provider's
// state dir via stageSessionFiles, then prompts to restart so the provider
// picks the session up at its staged-session apply on startup.
func cmdSessionLoad(p Provider, inFile string, force, dryRun, allowDiff bool) error {
	bundle, err := os.ReadFile(inFile)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("session file %q not found", inFile)
		}
		return err
	}
	pass, err := readPassphrase("Enter passphrase: ")
	if err != nil {
		return err
	}
	files, err := decryptUntar(string(bundle), pass)
	if err != nil {
		return fmt.Errorf("failed to decrypt session bundle (wrong passphrase or corrupt file): %v", err)
	}
	if !sessionHasJWT(files) {
		return errors.New("session bundle is missing 'jwt', not a valid session bundle")
	}
	// Reinstate the audit + confirm gate the other destructive commands share:
	// loading stages a full identity replacement. -n prints the plan and does
	// nothing; -f skips the prompt.
	ok, err := confirmGate("load session into "+providerLabel(p), p, force, dryRun)
	if err != nil {
		return err
	}
	if !ok {
		return nil // dry-run or declined: confirmGate printed the audit line
	}
	backupDir, err := stageSessionFiles(p, files, allowDiff)
	if err != nil {
		return err
	}
	fmt.Printf("Backed up current session to %s\n", backupDir)
	fmt.Println("Session staged.")

	if !readYesNo("Restart provider now to apply the loaded session? (Y/n):") {
		fmt.Println("Session staged. Run 'urnet-tools restart' when ready.")
		return nil
	}
	if p.Unit == "" {
		fmt.Println("Provider has no owning systemd unit; restart it manually.")
		return nil
	}
	if err := unitCommand(p, "restart"); err != nil {
		return fmt.Errorf("failed to restart %s: %v (session is staged; run 'urnet-tools restart')", providerLabel(p), err)
	}
	fmt.Printf("Restarted %s with the loaded session.\n", providerLabel(p))
	return nil
}
