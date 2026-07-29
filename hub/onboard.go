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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	onboardTokenTTL = 15 * time.Minute
)

// validHostname matches a bare hostname or IPv4 address: letters, digits, dots,
// and hyphens only. r.Host is client-controlled and gets interpolated into a
// double-quoted shell variable in onboardScriptTemplate, so anything outside
// this set (in particular `$`, `(`, `)`, which Go's Host-header validation
// permits) must be rejected rather than passed through.
var validHostname = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)

var tokenFileMu sync.Mutex

// mintOnboardToken creates a token, appends to onboard.tokens, prunes expired
func mintOnboardToken(dataDir string, now time.Time, ttl time.Duration) (token string, expiry time.Time, err error) {
	b := make([]byte, 32)
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

func tokenExists(dataDir, token string) bool {
	path := filepath.Join(dataDir, "onboard.tokens")
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, token+" ") {
			return true
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

// handleCACert serves GET/POST /api/ca-cert.
// Authenticates via either:
//  1. Bearer token matching URNETWORK_HUB_TOKEN (for auto-bootstrap), or
//  2. ?token= query param matching an onboard token (for first-time setup).
func handleCACert(dataDir string, ca *hubCA, hubToken string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Try Bearer auth with the hub token first
		authHeader := r.Header.Get("Authorization")
		if hubToken != "" && strings.HasPrefix(authHeader, "Bearer ") {
			given := strings.TrimPrefix(authHeader, "Bearer ")
			if subtle.ConstantTimeCompare([]byte(given), []byte(hubToken)) == 1 {
				serveCACert(w, ca)
				return
			}
		}

		// Fall back to onboard token query param
		token := r.URL.Query().Get("token")
		if token == "" {
			token = r.FormValue("token")
		}
		if token == "" {
			http.Error(w, `{"error":"missing token"}`, 403)
			return
		}
		if !validateOnboardToken(dataDir, token, time.Now()) {
			if tokenExists(dataDir, token) {
				http.Error(w, `{"error":"token expired"}`, 403)
			} else {
				http.Error(w, `{"error":"token not found"}`, 403)
			}
			return
		}
		tokenPrefix := token
		if len(tokenPrefix) > 8 {
			tokenPrefix = tokenPrefix[:8]
		}
		fmt.Printf("hub: onboard %s... from %s\n", tokenPrefix, r.RemoteAddr)
		serveCACert(w, ca)
	}
}

func serveCACert(w http.ResponseWriter, ca *hubCA) {
	w.Header().Set("Content-Type", "application/json")
	caFP, _ := ca.caFingerprint()
	json.NewEncoder(w).Encode(map[string]string{
		"ca_pem":         strings.TrimSpace(string(ca.certPEM)),
		"ca_fingerprint": caFP,
	})
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
		if !validHostname.MatchString(hostOnly) {
			http.Error(w, `{"error":"invalid host"}`, 400)
			return
		}
		httpsURL := fmt.Sprintf("https://%s", hostOnly)
		if tlsPort != "" && tlsPort != "443" {
			httpsURL = fmt.Sprintf("https://%s:%s", hostOnly, tlsPort)
		}

		w.Header().Set("Content-Type", "text/x-shellscript")
		fmt.Fprintf(w, onboardScriptTemplate, httpsURL)
	}
}

const onboardScriptTemplate = `#!/bin/sh
if [ -z "$1" ]; then
    echo "Usage: curl -fsSL http://HUB:8080/onboard.sh | sh -s -- TOKEN"
    exit 1
fi

TOKEN="$1"
HUB_URL="%s"

mkdir -p ~/.urnetwork

echo "Fetching CA certificate..."
RESPONSE=$(curl -fsSk "$HUB_URL/api/ca-cert?token=$TOKEN" 2>/dev/null || true)
if [ -z "$RESPONSE" ]; then
    RESPONSE=$(curl -fsS "http://$(echo "$HUB_URL" | sed 's|^https://||;s|:[0-9]*$||'):8080/api/ca-cert?token=$TOKEN" 2>/dev/null || true)
fi
if [ -z "$RESPONSE" ]; then
    echo "Primary fetch failed — falling back to direct cert retrieval..."
    RESPONSE=$(curl -fsSk "$HUB_URL/api/cert" 2>/dev/null || true)
fi

CA_PEM=""
CA_FP=""
if command -v jq >/dev/null 2>&1; then
    CA_PEM=$(echo "$RESPONSE" | jq -r '.ca_pem // ""' 2>/dev/null)
    CA_FP=$(echo "$RESPONSE" | jq -r '.ca_fingerprint // ""' 2>/dev/null)
fi
if [ -z "$CA_PEM" ] && command -v python3 >/dev/null 2>&1; then
    CA_PEM=$(echo "$RESPONSE" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('ca_pem',''))" 2>/dev/null)
    CA_FP=$(echo "$RESPONSE" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('ca_fingerprint',''))" 2>/dev/null)
fi
if [ -z "$CA_PEM" ]; then
    # awk fallback for bare Alpine/BusyBox (no jq, no python3)
    CA_PEM=$(echo "$RESPONSE" | awk -F'"' '{for(i=1;i<NF;i++) if($i=="ca_pem"){v=$(i+2); gsub(/\\\\n/,"\n",v); print v; exit}}' 2>/dev/null)
    CA_FP=$(echo "$RESPONSE" | awk -F'"' '{for(i=1;i<NF;i++) if($i=="ca_fingerprint"){print $(i+2); exit}}' 2>/dev/null)
fi
if [ -z "$CA_PEM" ]; then
    CA_PEM=$(echo "$RESPONSE" | sed -n 's/.*"ca_pem" *: *"\([^"]*\)".*/\1/p' | sed 's/\\n/\n/g')
    CA_FP=$(echo "$RESPONSE" | sed -n 's/.*"ca_fingerprint" *: *"\([^"]*\)".*/\1/p')
fi

if [ -z "$CA_PEM" ]; then
    echo "ERROR: Could not parse CA certificate"
    exit 1
fi

echo "CA fingerprint: $CA_FP"

# Write CA cert with retry. Use a heredoc rather than echo or printf:
# - echo on BusyBox ash (Alpine) mangles multi-line variables with special chars
# - printf interprets %% format sequences in the PEM body when bash history expansion is on
for _try in 1 2 3; do
    cat > ~/.urnetwork/hub_ca.pem.tmp <<-PEMEOF
$CA_PEM
PEMEOF
    # Verify the file has both PEM markers (not just the header)
    if head -1 ~/.urnetwork/hub_ca.pem.tmp | grep -q "BEGIN CERTIFICATE" && \
       tail -1 ~/.urnetwork/hub_ca.pem.tmp | grep -q "END CERTIFICATE"; then
        break
    fi
    echo "WARNING: CA cert appears truncated (missing END marker), retrying..."
    sleep 1
done
if ! head -1 ~/.urnetwork/hub_ca.pem.tmp | grep -q "BEGIN CERTIFICATE" || \
   ! tail -1 ~/.urnetwork/hub_ca.pem.tmp | grep -q "END CERTIFICATE"; then
    echo "ERROR: Failed to fetch complete CA certificate after 3 attempts"
    rm -f ~/.urnetwork/hub_ca.pem.tmp
    exit 1
fi
chmod 600 ~/.urnetwork/hub_ca.pem.tmp
mv ~/.urnetwork/hub_ca.pem.tmp ~/.urnetwork/hub_ca.pem

# Set report URL
echo "$HUB_URL" > ~/.urnetwork/report_url

# Clean up legacy pin
rm -f ~/.urnetwork/hub.pin

echo "Done. Hub CA installed and report URL configured."
`
