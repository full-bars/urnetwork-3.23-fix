package connect

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// What the activator logs when an activation post fails (connect#229).
//
// A post whose every attempt failed ends in an exhausted request, whose own
// message is the fixed "http request attempts exhausted": logged alone, it
// hides which candidate failed and how. The log line names the cause of each
// attempt after it, and never the jwt the post carried.
//
// The post runs through a real direct strategy, as the role builds one, whose
// tcp dial and udp socket each fail at once with their own cause, so the
// normal dialer and the two alt dialers fail on every attempt. It runs in a
// synctest bubble: the request budget ends in bubble time once every attempt
// has failed, with no wait and no network.

// Records every line the activator logs.
type testActivateLogger struct {
	stateLock sync.Mutex
	lines     []string
}

func (self *testActivateLogger) add(line string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, line)
}

func (self *testActivateLogger) Info(args ...any) {
	self.add(fmt.Sprint(args...))
}

func (self *testActivateLogger) Infof(format string, args ...any) {
	self.add(fmt.Sprintf(format, args...))
}

func (self *testActivateLogger) Warningf(format string, args ...any) {
	self.add(fmt.Sprintf(format, args...))
}

func (self *testActivateLogger) Errorf(format string, args ...any) {
	self.add(fmt.Sprintf(format, args...))
}

func (self *testActivateLogger) V(level int32) Verbose {
	return NewNoopLogger().V(level)
}

// The logged lines that start with `prefix`.
func (self *testActivateLogger) linesWithPrefix(prefix string) []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	lines := []string{}
	for _, line := range self.lines {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, line)
		}
	}
	return lines
}

// The logged lines that contain `sub`. A line that starts with an emoji cannot
// be found by prefix without repeating the emoji in every assertion.
func (self *testActivateLogger) linesContaining(sub string) []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	lines := []string{}
	for _, line := range self.lines {
		if strings.Contains(line, sub) {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestActivationLogTextRedactsCredentialsAndNewlines(t *testing.T) {
	token := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJzZWNyZXQifQ.signatureSecret123"
	text := activationLogText("bad\nhttps://user:pass@example.org?token=hidden "+token+" test-jwt", "test-jwt", "https://user:pass@example.org?token=hidden")
	for _, secret := range []string{token, "test-jwt", "pass", "hidden", "\n"} {
		if strings.Contains(text, secret) {
			t.Fatalf("secret or newline in log: %q", text)
		}
	}
	if !strings.Contains(text, "example.org") {
		t.Fatal(text)
	}
}

// http.Client.Do masks the password in the url of its error and appends the
// request path, so the text no longer contains the configured api url (js/wasm
// returns that error as is).
func TestActivationLogTextSanitizesRedactedPathAppendedUrl(t *testing.T) {
	text := activationLogText(
		`http request failed: Post "https://user:xxxxx@example.org/v1/extender/activate?token=hidden#frag": EOF`,
		"test-jwt",
		"https://user:pass@example.org?token=hidden",
	)
	for _, secret := range []string{"user", "xxxxx", "hidden", "frag", "/v1/extender"} {
		if strings.Contains(text, secret) {
			t.Fatalf("%q leaked into log: %q", secret, text)
		}
	}
	if !strings.Contains(text, "example.org") || !strings.Contains(text, "EOF") {
		t.Fatalf("log lost the host or the cause: %q", text)
	}
}

func TestExtenderActivatorRefusalLog(t *testing.T) {
	const jwt = "sensitive-test-jwt"
	operator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/hello" {
			fmt.Fprint(w, `{"client_address":"127.0.0.1"}`)
			return
		}
		fmt.Fprint(w, `{"activated":false,"ip_version":4,"retry_after_seconds":95,"error":"quic carrier did not answer: sensitive-test-jwt"}`)
	}))
	defer operator.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	strategySettings := DefaultClientStrategySettings()
	strategySettings.Log = NewNoopLogger()
	strategySettings.ConnectSettings.Log = NewNoopLogger()
	strategySettings.EnableResilient = false
	strategy := NewDirectClientStrategy(ctx, strategySettings, 0)
	defer strategy.Close()
	logger := &testActivateLogger{}
	settings := DefaultExtenderActivatorSettings()
	settings.Log = logger
	settings.ApiUrlV4 = operator.URL
	settings.ByJwt = func() string { return jwt }
	settings.ClientStrategy = strategy
	settings.Carriers = func() []string { return []string{ExtenderCarrierTcp, ExtenderCarrierQuic} }
	settings.IpVersionSupported = func(int) bool { return true }
	settings.AddNetworkChangeListener = func(func()) func() { return func() {} }
	activator := NewExtenderActivator(ctx, settings)
	defer activator.Close()
	for {
		version, changed := activator.ChangeMonitor().Get()
		if version > 0 {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	lines := logger.linesContaining("refused")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "[extender] activate v4 refused ") {
		t.Fatalf("refusal lines: %v", lines)
	}
	line := lines[0]
	for _, want := range []string{"carriers=tcp,quic", "api=127.0.0.1:", "last_remote=127.0.0.1:", "elapsed=", "attempts=", "retry_after=1m35s", "quic carrier did not answer", "[redacted]"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q from %q", want, line)
		}
	}
	if strings.Contains(line, jwt) {
		t.Fatal("jwt leaked")
	}
	// the recorded failure carries the same operator text as the log, so it
	// must be sanitized too (Status().Family(4).LastError is what a diagnostic
	// reader sees later)
	if family := activator.Status().Family(4); family != nil && strings.Contains(family.LastError, jwt) {
		t.Fatalf("jwt leaked into the recorded failure: %q", family.LastError)
	}
	t.Logf("refusal log: %s", line)
}

// The literal apiURL replacement matches a bare substring. If it runs before
// the url matcher, "<apiURL>?code=secret" becomes "<host>?code=secret": the
// scheme is gone, the matcher no longer sees a url, and the query survives.
// This is the ordering the activation path had before the url strip moved
// ahead of it.
func TestActivationLogTextStripsUrlBeforeLiteralApiUrl(t *testing.T) {
	apiURL := "https://api.example.org/v1/extender/activate"
	text := activationLogText(
		`http request failed: Post "`+apiURL+`?code=query-secret#frag": EOF`,
		"test-jwt",
		apiURL,
	)
	for _, secret := range []string{"query-secret", "frag", "?"} {
		if strings.Contains(text, secret) {
			t.Fatalf("url component %q survived sanitization: %q", secret, text)
		}
	}
	if !strings.Contains(text, "api.example.org") {
		t.Fatalf("the host should survive: %q", text)
	}
}

// activationLogError is the no-jwt sanitizer used on the hello path.
func TestActivationLogErrorStripsUrlAndJwtShape(t *testing.T) {
	token := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4eXoifQ.c2lnbmF0dXJlLXZhbHVl"
	text := activationLogError(`Post "https://user:pass@example.org/hello?token=hidden": ` + token)
	for _, secret := range []string{"hidden", "pass", token} {
		if strings.Contains(text, secret) {
			t.Fatalf("secret %q survived: %q", secret, text)
		}
	}
}

// A quote inside a query value used to truncate the url match, and the tail
// after the quote escaped the strip.
func TestActivationLogTextHandlesQuoteInsideQuery(t *testing.T) {
	// the quote sits INSIDE the query value, so a matcher that stops at a quote
	// truncates the url and leaves the rest of the secret in the text
	text := activationLogText(
		`http request failed: Post "https://api.example.org/a?code=quo"ted-secret": EOF`,
		"test-jwt",
		"https://api.example.org/a",
	)
	for _, fragment := range []string{"quo", "ted-secret", "code="} {
		if strings.Contains(text, fragment) {
			t.Fatalf("query fragment %q survived: %q", fragment, text)
		}
	}
	if !strings.Contains(text, "api.example.org") {
		t.Fatalf("host should survive: %q", text)
	}
}

// A scheme-less host carrying a query is a leftover of the literal apiURL
// replacement; it is stripped by name, so the apiURL must be the one in play.
func TestActivationLogTextStripsSchemalessQuery(t *testing.T) {
	text := activationLogText("left api.example.org/a?code=scheme-less-secret behind", "test-jwt", "https://api.example.org/a")
	if strings.Contains(text, "scheme-less-secret") {
		t.Fatalf("scheme-less query survived: %q", text)
	}
}

// The refusal message is operator-supplied text and is persisted, so it needs
// the same treatment as a transport error.
func TestActivationLogTextSanitizesRefusalMessage(t *testing.T) {
	text := activationLogText("refused: https://api.example.org/activate?token=refusal-secret", "test-jwt", "")
	for _, secret := range []string{"refusal-secret", "token="} {
		if strings.Contains(text, secret) {
			t.Fatalf("%q survived in refusal text: %q", secret, text)
		}
	}
}

// IP-literal endpoints are redacted by name too, bracketed IPv6 included.
func TestActivationLogTextStripsSchemalessIpLiteral(t *testing.T) {
	for _, c := range []struct{ text, apiURL string }{
		{"left 1.2.3.4?code=ip-secret behind", "http://1.2.3.4"},
		{"left [::1]?code=ip-secret behind", "http://[::1]"},
		{"left [2001:db8::1]:8443/x?code=ip-secret behind", "https://[2001:db8::1]:8443/activate"},
	} {
		got := activationLogText(c.text, "", c.apiURL)
		if strings.Contains(got, "ip-secret") {
			t.Fatalf("query survived for %q: %q", c.text, got)
		}
	}
}

// A port in the host used to break the structural match and leak the tail.
func TestActivationLogTextStripsHostWithPort(t *testing.T) {
	got := activationLogText("left api.example.org:8443/x?code=port-secret behind", "", "https://api.example.org:8443/activate")
	if strings.Contains(got, "port-secret") || strings.Contains(got, "/x") {
		t.Fatalf("port host tail survived: %q", got)
	}
	if got != "left api.example.org:8443 behind" {
		t.Fatalf("unexpected output: %q", got)
	}
}

// Text that is not the api host is left alone: there is no pattern to
// misfire on addresses, stack traces or measurements.
func TestActivationLogTextLeavesOtherTextAlone(t *testing.T) {
	const apiURL = "https://api.example.org/activate"
	for _, in := range []string{
		"notify ops@example.org or user@host about it",
		"goroutine 5 [running]: main.(*Foo).Bar(0xc000012345) /src/app/foo.go:42 +0x1f",
		"net/http.(*Transport).RoundTrip? failed after 1.5s? retry",
	} {
		if got := activationLogText(in, "", apiURL); got != in {
			t.Fatalf("diagnostic mangled:\n  in:  %q\n  out: %q", in, got)
		}
	}
}

// Diagnostics must survive the sanitizer: a measurement or a version is not a
// host, and a permissive matcher replaced them with [url].
func TestActivationLogTextKeepsMeasurements(t *testing.T) {
	const in = "elapsed 1.5s? then v2.0? and step 1.2? done"
	if got := activationLogText(in, "", ""); got != in {
		t.Fatalf("measurements mangled:\n  in:  %q\n  out: %q", in, got)
	}
}

// A schemaless api host with a path and no query still hides a secret in the path.
func TestActivationLogTextStripsSchemalessPath(t *testing.T) {
	got := activationLogText("error at api.example.org/v1/keys/SECRET12345/view failed", "", "https://api.example.org/activate")
	if strings.Contains(got, "SECRET12345") {
		t.Fatalf("path secret survived: %q", got)
	}
	if !strings.Contains(got, "api.example.org") {
		t.Fatalf("host should survive: %q", got)
	}
}

// Userinfo on a scheme'd url is dropped with the rest of the url. The bare
// "user:pass@ip?query" form has no scheme and no known host to key on; the old
// test asserted a structural match that no longer exists, so it now asserts the
// scheme'd form that the url stripper still owns.
func TestActivationLogTextStripsUserinfoBeforeIpLiteral(t *testing.T) {
	got := activationLogText("connect to http://admin:supersecret@1.2.3.4/x?token=secret failed", "", "")
	for _, secret := range []string{"supersecret", "token=secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("%q survived: %q", secret, got)
		}
	}
}
