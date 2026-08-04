package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	caSubjectCN       = "urnetwork-hub-ca"
	caNotBefore       = "2026-01-01T00:00:00Z"
	caValidYears      = 100
	leafValidHours    = 48
	leafRotationHours = 24
	argonTime         = 3
	argonMemory       = 64 * 1024 // 64 MiB
	argonThreads      = 4
	argonKeyLen       = 32
	leafSANMax        = 64
)

type hubCA struct {
	key     ed25519.PrivateKey
	cert    *x509.Certificate
	certPEM []byte
}

func deriveCASeed(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
}

func deriveCA(password string, salt []byte) (*hubCA, error) {
	seed := deriveCASeed(password, salt)
	key := ed25519.NewKeyFromSeed(seed)

	serialBytes, err := hkdfExpandSHA256(seed, "urnetwork-hub-ca-serial", 16)
	if err != nil {
		return nil, fmt.Errorf("derive serial: %w", err)
	}
	serial := new(big.Int).SetBytes(serialBytes)

	notBefore, err := time.Parse(time.RFC3339, caNotBefore)
	if err != nil {
		return nil, fmt.Errorf("parse caNotBefore: %w", err)
	}
	notAfter := notBefore.AddDate(caValidYears, 0, 0)

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: caSubjectCN},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
		MaxPathLenZero:        true,
		MaxPathLen:            0,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, fmt.Errorf("create CA cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}

	return &hubCA{key: key, cert: cert, certPEM: certPEM}, nil
}

func (ca *hubCA) caFingerprint() (string, error) {
	cert := pemDecodeCert(ca.certPEM)
	if cert == nil {
		return "", fmt.Errorf("decode CA certificate PEM for fingerprint")
	}
	return fmt.Sprintf("SHA256:%x", sha256.Sum256(cert)), nil
}

func (ca *hubCA) issueLeaf(sans []string, validity time.Duration) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate leaf key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("leaf serial: %w", err)
	}

	var dnsNames []string
	var ipAddrs []net.IP
	for _, san := range sans {
		if ip := net.ParseIP(san); ip != nil {
			ipAddrs = append(ipAddrs, ip)
		} else {
			dnsNames = append(dnsNames, san)
		}
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "urnetwork-hub-leaf"},
		NotBefore:    now,
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ipAddrs,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create leaf cert: %w", err)
	}

	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal leaf key: %w", err)
	}

	return tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}),
	)
}

func hkdfExpandSHA256(seed []byte, info string, length int) ([]byte, error) {
	return hkdf.Expand(sha256.New, seed, info, length)
}

func leafSANs() []string {
	var sans []string
	if h, _ := os.Hostname(); h != "" {
		sans = append(sans, h)
	}
	sans = append(sans, "localhost", "127.0.0.1", "::1")
	if ifis, err := net.Interfaces(); err == nil {
		for _, ifi := range ifis {
			if ifi.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, _ := ifi.Addrs()
			for _, addr := range addrs {
				if ip, ok := addr.(*net.IPNet); ok && !ip.IP.IsLoopback() {
					sans = append(sans, ip.IP.String())
				}
			}
		}
	}
	if extra := os.Getenv("URNETWORK_HUB_TLS_NAMES"); extra != "" {
		for _, name := range strings.Split(extra, ",") {
			if n := strings.TrimSpace(name); n != "" {
				sans = append(sans, n)
			}
		}
	}
	if len(sans) > leafSANMax {
		sans = sans[:leafSANMax]
	}
	return sans
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func loadOrCreateCAMaterial(dataDir string) (password string, salt []byte, generatedPassword bool, err error) {
	sweepStaleTmpFiles(dataDir)

	passwordPath := filepath.Join(dataDir, "hub.password")
	saltPath := filepath.Join(dataDir, "hub.salt")

	pwExists := fileExists(passwordPath)
	saltExists := fileExists(saltPath)

	// Cross-consistency guard: if one file exists but the other doesn't, the
	// missing half was likely deleted accidentally. Regenerating either half
	// in isolation produces a completely different CA root, silently breaking
	// every provider's trust chain. Treat this as corruption.
	if pwExists != saltExists {
		return "", nil, false, fmt.Errorf(
			"CA material inconsistency: password exists=%v salt exists=%v — "+
				"both files must exist or neither. If you intend to reset the CA, "+
				"delete both %s and %s and restart",
			pwExists, saltExists, passwordPath, saltPath,
		)
	}

	if pwExists {
		data, _ := os.ReadFile(passwordPath)
		password = strings.TrimSpace(string(data))
	} else {
		b := make([]byte, 24)
		if _, readErr := io.ReadFull(rand.Reader, b); readErr != nil {
			return "", nil, false, fmt.Errorf("generate password: %w", readErr)
		}
		password = base64.RawURLEncoding.EncodeToString(b)
		if writeErr := writeFileAtomic(passwordPath, []byte(password+"\n"), 0600); writeErr != nil {
			return "", nil, false, fmt.Errorf("write password: %w", writeErr)
		}
		generatedPassword = true
	}

	if data, readErr := os.ReadFile(saltPath); readErr == nil {
		salt, err = hex.DecodeString(strings.TrimSpace(string(data)))
		if err != nil {
			return password, nil, generatedPassword, fmt.Errorf("decode salt: %w", err)
		}
	} else {
		salt = make([]byte, 32)
		if _, readErr := rand.Read(salt); readErr != nil {
			return password, nil, generatedPassword, fmt.Errorf("generate salt: %w", readErr)
		}
		if writeErr := writeFileAtomic(saltPath, []byte(hex.EncodeToString(salt)+"\n"), 0600); writeErr != nil {
			return password, nil, generatedPassword, fmt.Errorf("write salt: %w", writeErr)
		}
	}

	if len(password) < 8 {
		return password, nil, generatedPassword, fmt.Errorf(
			"hub password must be at least 8 characters (got %d)", len(password))
	}

	return password, salt, generatedPassword, nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sweepStaleTmpFiles(dataDir string) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tmp") {
			os.Remove(filepath.Join(dataDir, e.Name()))
		}
	}
}
