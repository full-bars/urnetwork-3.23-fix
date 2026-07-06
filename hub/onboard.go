package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	onboardTokenTTL = 15 * time.Minute
)

var tokenFileMu sync.Mutex

// mintOnboardToken creates a token, appends to onboard.tokens, prunes expired
func mintOnboardToken(dataDir string, now time.Time, ttl time.Duration) (token string, expiry time.Time, err error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, fmt.Errorf("generate token: %w", err)
	}
	token = hex.EncodeToString(b)
	expiry = now.Add(ttl)

	tokenFileMu.Lock()
	defer tokenFileMu.Unlock()

	path := filepath.Join(dataDir, "onboard.tokens")
	pruneExpiredLines(path, now)

	line := fmt.Sprintf("%s %d\n", token, expiry.Unix())
	if err := appendToFile(path, []byte(line)); err != nil {
		return "", time.Time{}, fmt.Errorf("write token: %w", err)
	}

	return token, expiry, nil
}

// validateOnboardToken checks if token is valid and unexpired
func validateOnboardToken(dataDir, token string, now time.Time) bool {
	tokenFileMu.Lock()
	defer tokenFileMu.Unlock()

	path := filepath.Join(dataDir, "onboard.tokens")
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(parts[0]), []byte(token)) == 1 {
			expiryUnix, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				continue
			}
			if now.Before(time.Unix(expiryUnix, 0)) {
				return true
			}
		}
	}
	return false
}

func pruneExpiredLines(path string, now time.Time) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	var valid []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		expiryUnix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		if now.Before(time.Unix(expiryUnix, 0)) {
			valid = append(valid, line)
		}
	}

	os.WriteFile(path, []byte(strings.Join(valid, "\n")+"\n"), 0600)
}

func appendToFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

// handleCACert serves GET/POST /api/ca-cert
func handleCACert(dataDir string, ca *hubCA) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if token == "" {
			token = r.FormValue("token")
		}
		if token == "" {
			http.Error(w, `{"error":"missing token"}`, 403)
			return
		}
		if !validateOnboardToken(dataDir, token, time.Now()) {
			http.Error(w, `{"error":"invalid or expired token"}`, 403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"ca_pem":         strings.TrimSpace(string(ca.certPEM)),
			"ca_fingerprint": ca.caFingerprint(),
		})
	}
}

// handleOnboardScript serves GET /onboard.sh
func handleOnboardScript(tlsPort string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// r.Host carries whatever port the request actually arrived on
		// (e.g. the plain-HTTP port from the documented curl one-liner),
		// which is unrelated to tlsPort — always take just the hostname
		// and append tlsPort explicitly, rather than falling back to the
		// request's own host:port when tlsPort happens to be "443".
		hostOnly := strings.Split(r.Host, ":")[0]
		httpsURL := fmt.Sprintf("https://%s", hostOnly)
		if tlsPort != "" && tlsPort != "443" {
			httpsURL = fmt.Sprintf("https://%s:%s", hostOnly, tlsPort)
		}

		w.Header().Set("Content-Type", "text/x-shellscript")
		fmt.Fprintf(w, onboardScriptTemplate, httpsURL)
	}
}

const onboardScriptTemplate = `#!/bin/sh
set -e
if [ -z "$1" ]; then
    echo "Usage: curl -fsSL http://HUB:8080/onboard.sh | sh -s -- TOKEN"
    exit 1
fi

TOKEN="$1"
HUB_URL="%s"

mkdir -p ~/.urnetwork

echo "Fetching CA certificate..."
RESPONSE=$(curl -fsSk "$HUB_URL/api/ca-cert?token=$TOKEN" 2>/dev/null || curl -fsS "http://$(echo $HUB_URL | sed 's/https:\/\///'):8080/api/ca-cert?token=$TOKEN")

CA_PEM=$(echo "$RESPONSE" | sed -n 's/.*"ca_pem" *: *"\([^"]*\)".*/\1/p' | sed 's/\\n/\n/g')
CA_FP=$(echo "$RESPONSE" | sed -n 's/.*"ca_fingerprint" *: *"\([^"]*\)".*/\1/p')

if [ -z "$CA_PEM" ]; then
    echo "ERROR: Could not parse CA certificate"
    exit 1
fi

echo "CA fingerprint: $CA_FP"

# Write CA cert. CA_PEM is already the actual PEM text (BEGIN/END markers
# and base64 body), not a base64-encoded blob of the whole thing — write it
# directly. Piping it through "base64 -d" is wrong here and dangerous on
# BusyBox/Alpine specifically: BusyBox's base64 -d silently ignores invalid
# characters (like the "-----BEGIN CERTIFICATE-----" header) and exits 0
# with corrupted output instead of failing, so a "|| fallback" pattern would
# never trigger and hub_ca.pem would end up as garbage on exactly the
# fork's documented Docker/Alpine deployment target.
echo "$CA_PEM" > ~/.urnetwork/hub_ca.pem.tmp
mv ~/.urnetwork/hub_ca.pem.tmp ~/.urnetwork/hub_ca.pem
chmod 600 ~/.urnetwork/hub_ca.pem

# Set report URL
echo "$HUB_URL" > ~/.urnetwork/report_url

# Clean up legacy pin
rm -f ~/.urnetwork/hub.pin

echo "Done. Hub CA installed and report URL configured."
`
