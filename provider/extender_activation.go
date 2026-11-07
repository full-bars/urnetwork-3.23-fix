package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// extender_activation.go attaches the activation loop (net_extender_activate.go)
// to the extender role.
//
// Listening is not enough: the platform sends censored clients only to an
// extender it has listed, and it lists one only after it proves the extender
// itself, by dialing back to the host's own address on tcp 443, requiring a
// signed answer and forwarding a real request through it (server
// controller/extender_controller.go, the required test). The activation loop
// posts that request for each address family, renews it and backs off as the
// platform asks.
//
// The loop needs three things that exist only once the direct slot has
// authenticated, which is after the role starts: the api url, the live client
// JWT and a strategy that only ever dials direct. They arrive as
// extenderActivationInputs, attached late, and detached when the slot ends.

// extenderActivationInputs is what the direct slot supplies.
type extenderActivationInputs struct {
	// ctx is the direct slot's context. The activation ends when it does, so
	// `provider direct off` stops it and `direct on` starts a fresh one.
	ctx    context.Context
	apiUrl string
	// byJwt returns the slot's CURRENT client JWT. It is a getter, never a
	// value: the watcher renews the token about every 12 hours and an
	// activation can fall a day after the last one.
	byJwt func() string
	// clientStrategy must dial direct and never through a proxy or an
	// extender: the platform publishes the address it sees, so an activation
	// that crossed one would list the wrong host.
	clientStrategy *connect.ClientStrategy
}

// jwtHolder is the value the JWT renewal watcher updates and the activation
// reads. An atomic pointer keeps the read lock-free on the activation path.
type jwtHolder struct {
	value atomic.Pointer[string]
}

func newJwtHolder(initial string) *jwtHolder {
	h := &jwtHolder{}
	h.Set(initial)
	return h
}

func (h *jwtHolder) Set(jwt string) { h.value.Store(&jwt) }

func (h *jwtHolder) Get() string {
	if p := h.value.Load(); p != nil {
		return *p
	}
	return ""
}

// extenderPacingFileName holds when each family may next post and how its last
// attempt ended, so a restart or a reboot resumes the cadence the platform's
// record set instead of posting both families at once.
const extenderPacingFileName = "extender_pacing.json"

// extenderPacingFileStore is connect.ExtenderActivationPacingStore over a JSON
// file in the provider state directory.
type extenderPacingFileStore struct {
	path string
}

var _ connect.ExtenderActivationPacingStore = extenderPacingFileStore{}

func newExtenderPacingFileStore() (extenderPacingFileStore, error) {
	path, err := providerStatePath(extenderPacingFileName)
	if err != nil {
		return extenderPacingFileStore{}, err
	}
	return extenderPacingFileStore{path: path}, nil
}

// LoadPacing returns the stored pacing, nil when none was saved yet. A file
// that cannot be read is an error, which starts the loop as a new process
// would; the next save replaces it.
func (s extenderPacingFileStore) LoadPacing() (*connect.ExtenderActivationPacingRecord, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record connect.ExtenderActivationPacingRecord
	if err := json.Unmarshal(b, &record); err != nil {
		return nil, fmt.Errorf("%s is not a valid pacing record: %w", s.path, err)
	}
	return &record, nil
}

func (s extenderPacingFileStore) SavePacing(record *connect.ExtenderActivationPacingRecord) error {
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return atomicWriteFile(s.path, b, 0600)
}

// extenderActivatorLogger sends the activation loop's lines into the provider
// log. The loop already writes plain sentences with a 🌉 mark and the
// `[extender]` tag, which is an important-log marker. Outcomes (activated,
// failed, refused) are also written to the disk events log, so they survive a
// reboot wiping the ramlog; the five-minute "still not activated" reminder goes
// through the ordinary log only. That still reaches the RAM log and the bounded,
// rotated important log (the `[extender]` tag is an important marker) but not the
// events log, so a long failure cannot fill it.
// extenderReminderLine matches the loop's five-minute "still not activated" line,
// which is a family name and a status mark after the bridge. The family is a
// digit rather than 4 or 6: with no per-family api host the loop tracks one
// shared post as family 0 and names it IPv0. Every other line the loop writes is
// an outcome and also goes to the disk events log.
var extenderReminderLine = regexp.MustCompile(`^🌉 \[extender\] IPv\d (✅|⛔|❌|⏳)`)

type extenderActivatorLogger struct{}

var _ connect.Logger = extenderActivatorLogger{}

func (extenderActivatorLogger) emit(line string) {
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	if extenderReminderLine.MatchString(line) {
		tlog("%s", line)
		return
	}
	importantLogf("%s", line)
}

func (l extenderActivatorLogger) Info(args ...any) { l.emit(fmt.Sprint(args...)) }

func (l extenderActivatorLogger) Infof(format string, args ...any) {
	l.emit(fmt.Sprintf(format, args...))
}

func (l extenderActivatorLogger) Warningf(format string, args ...any) {
	l.emit("⚠️ " + fmt.Sprintf(format, args...))
}

func (l extenderActivatorLogger) Errorf(format string, args ...any) {
	l.emit("❌ " + fmt.Sprintf(format, args...))
}

func (extenderActivatorLogger) V(level int32) connect.Verbose { return quietVerbose{} }

type quietVerbose struct{}

func (quietVerbose) Enabled() bool        { return false }
func (quietVerbose) Info(args ...any)     {}
func (quietVerbose) Infof(string, ...any) {}

// activatorSettings builds the loop's settings from the role's live state.
// Carriers and DnsPorts are closures over the role, read under its lock at
// every pass, because there can be two servers (the primary and a tcp retry)
// and the set changes while the loop runs.
// extenderDirectoryFileName holds the platform's signed records the directory has
// verified. The activator trusts a stored activation after a restart only when the
// directory still holds the record, so without this file every restart would clear it.
const extenderDirectoryFileName = "extender_directory.json"

// extenderDirectoryFileStore is connect.ExtenderDirectoryStore over a JSON file in the
// provider state directory.
type extenderDirectoryFileStore struct {
	path string
}

var _ connect.ExtenderDirectoryStore = extenderDirectoryFileStore{}

func (s extenderDirectoryFileStore) Load() ([]byte, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

func (s extenderDirectoryFileStore) Save(b []byte) error {
	return atomicWriteFile(s.path, b, 0600)
}

// extenderFixedRenewalInterval is the renewal period used while the platform's root
// keys are not loaded. The platform's record lasts 24 hours, so a 24 hour period
// renews with about ten seconds to spare and a slow post lets the listing lapse.
const extenderFixedRenewalInterval = 12 * time.Hour

// How often the root keys are read again once loaded, and the retry steps while
// they cannot be read.
const (
	extenderRootKeysRefresh = 12 * time.Hour
	extenderRootKeysTimeout = 30 * time.Second
	// the most a start waits for the keys before the activator is built
	extenderRootKeysPrimeTimeout = 5 * time.Second
	extenderRootKeysRetryMin     = 1 * time.Minute
	extenderRootKeysRetryMax     = 30 * time.Minute
)

// rootKeyDirectory is the role's directory of platform root keys, created once.
//
// It accepts records only for the platform's own space host (the api host without
// its api/api-v4/api-v6 label, so api.bringyour.com is bringyour.com). Without that
// it rejects every record the platform returns, one log line each.
func (r *extenderRole) rootKeyDirectory(apiUrl string) *connect.ExtenderDirectory {
	r.directoryOnce.Do(func() {
		settings := connect.DefaultExtenderDirectorySettings()
		if apiHost, err := connect.ExtenderApiHostName(apiUrl); err == nil && net.ParseIP(apiHost) == nil {
			settings.NetworkHosts = []string{connect.ExtenderNetworkHostName(apiHost)}
		}
		if path, err := providerStatePath(extenderDirectoryFileName); err != nil {
			tlog("🌉⚠️ [extender] the platform's records will not survive a restart: no state directory: %v\n", err)
		} else {
			settings.Store = extenderDirectoryFileStore{path: path}
		}
		r.directory = connect.NewExtenderDirectory(r.ctx, settings)
	})
	return r.directory
}

// parseRootKeys reads the hex root keys out of the platform's /hello answer.
func parseRootKeys(helloBytes []byte) (*connect.ExtenderRootKeySet, error) {
	hello := &struct {
		ExtenderRootPublicKeys []string `json:"extender_root_public_keys"`
	}{}
	if err := json.Unmarshal(helloBytes, hello); err != nil {
		return nil, err
	}
	if len(hello.ExtenderRootPublicKeys) == 0 {
		return nil, errors.New("the platform published no extender root keys")
	}
	return connect.NewExtenderRootKeySetFromHex(hello.ExtenderRootPublicKeys...)
}

func (r *extenderRole) readRootKeys(inputs *extenderActivationInputs) (*connect.ExtenderRootKeySet, error) {
	ctx, cancel := context.WithTimeout(inputs.ctx, extenderRootKeysTimeout)
	defer cancel()
	var body []byte
	var err error
	if r.config.fetchHello != nil {
		body, err = r.config.fetchHello(ctx, inputs)
	} else {
		var request *http.Request
		request, err = connect.HelloRequestFromUrl(ctx, inputs.apiUrl, inputs.byJwt())
		if err == nil {
			body, err = connect.HttpGetWithStrategyRaw(ctx, inputs.clientStrategy, request.URL.String(), inputs.byJwt())
		}
	}
	if err != nil {
		return nil, err
	}
	return parseRootKeys(body)
}

// loadRootKeys reads the platform's root keys from /hello and installs them, then
// reads them again every few hours. It ends with the slot. A failure is logged once
// per streak and retried with a growing wait; the fixed renewal interval covers the gap.
func (r *extenderRole) loadRootKeys(inputs *extenderActivationInputs) {
	directory := r.rootKeyDirectory(inputs.apiUrl)
	wait := extenderRootKeysRetryMin
	failing := false
	for {
		keySet, err := r.readRootKeys(inputs)
		next := extenderRootKeysRefresh
		if inputs.ctx.Err() != nil || r.ctx.Err() != nil {
			// the slot ended (a startup restart, `direct off`): that is not a failure
			return
		}
		if err != nil {
			next = wait
			if wait *= 2; extenderRootKeysRetryMax < wait {
				wait = extenderRootKeysRetryMax
			}
			if !failing {
				failing = true
				var status *connect.HttpStatusError
				reason := "request failed"
				if errors.As(err, &status) && status.StatusCode >= 100 && status.StatusCode <= 599 {
					reason = fmt.Sprintf("HTTP %d", status.StatusCode)
				}
				importantLogf("🌉⚠️ [extender] could not read the platform's root keys (%s): renewals use a fixed %s interval until they load. Trying again in %s.\n",
					reason, extenderFixedRenewalInterval, next)
			}
		} else {
			if directory.RootKeys().Len() == 0 || failing {
				importantLogf("🌉✅ [extender] platform root keys loaded (%d): renewals now follow each signed record's lifetime.\n", keySet.Len())
			}
			directory.SetRootKeys(keySet)
			failing = false
			wait = extenderRootKeysRetryMin
		}
		timer := time.NewTimer(next)
		select {
		case <-inputs.ctx.Done():
			timer.Stop()
			return
		case <-r.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// primeRootKeys reads the root keys once, for at most `limit`, before the activator
// starts, so the stored activation can be checked against the verified directory. A
// failure is silent: the background loader reports it and retries.
func (r *extenderRole) primeRootKeys(inputs *extenderActivationInputs, limit time.Duration) {
	directory := r.rootKeyDirectory(inputs.apiUrl)
	if 0 < directory.RootKeys().Len() {
		return
	}
	done := make(chan *connect.ExtenderRootKeySet, 1)
	go func() {
		keySet, err := r.readRootKeys(inputs)
		if err != nil {
			keySet = nil
		}
		done <- keySet
	}()
	select {
	case keySet := <-done:
		if keySet != nil {
			directory.SetRootKeys(keySet)
			importantLogf("🌉✅ [extender] platform root keys loaded (%d): renewals now follow each signed record's lifetime.\n", keySet.Len())
		}
	case <-time.After(limit):
	case <-inputs.ctx.Done():
	}
}

// extenderRestartPostSpread is the window a restarted extender spreads its refresh post over.
const extenderRestartPostSpread = 30 * time.Minute

func (r *extenderRole) activatorSettings(inputs *extenderActivationInputs) *connect.ExtenderActivatorSettings {
	settings := connect.DefaultExtenderActivatorSettings()
	settings.Log = extenderActivatorLogger{}
	settings.ApiUrl = inputs.apiUrl
	settings.ApiUrlV4 = familyServiceUrl(inputs.apiUrl, 4)
	settings.ApiUrlV6 = familyServiceUrl(inputs.apiUrl, 6)
	settings.HelloUrl = inputs.apiUrl
	settings.ByJwt = inputs.byJwt
	settings.ClientStrategy = inputs.clientStrategy
	settings.PublicKey = r.identityPublicKey()
	settings.Carriers = r.activationCarriers
	settings.DnsPorts = r.activationDnsPorts
	// After a restart a still-valid listing is posted once more at a random moment
	// within this window, so many hosts that restart together do not all post at
	// the same time. It is half of connect's default window of one hour.
	settings.ReplacedPostJitter = extenderRestartPostSpread
	// The platform's root keys let the activator renew from the signed record's own
	// lifetime. Until they are loaded it renews on this fixed interval, which must
	// stay well inside the record's 24 hours (the platform issues exactly 24h).
	settings.ActivateTimeout = extenderFixedRenewalInterval
	settings.Directory = r.rootKeyDirectory(inputs.apiUrl)
	if store, err := newExtenderPacingFileStore(); err != nil {
		tlog("🌉⚠️ [extender] activation pacing will not survive a restart: no state directory: %v\n", err)
	} else {
		settings.PacingStore = store
	}
	settings.OnRefused = func(ipVersion int, result *connect.ExtenderActivateResult) {
		if result != nil && result.KeyHeldByAnotherNetwork() {
			importantLogf("🌉⛔ [extender] %s: the platform says this extender key belongs to ANOTHER network, so this host cannot be listed. "+
				"This happens when the identity file %s was copied from another box or account. Fix: stop the provider, delete that file so a new key is made for this network, and start it again.\n",
				extenderFamilyLabel(ipVersion), extenderKeyFileName)
		}
	}
	if r.config.configureActivator != nil {
		r.config.configureActivator(settings)
	}
	return settings
}

func extenderFamilyLabel(ipVersion int) string {
	if ipVersion == 6 {
		return "IPv6"
	}
	return "IPv4"
}

// identityPublicKey is the role's identity public key, which the platform
// probes back against and signs the record for.
func (r *extenderRole) identityPublicKey() []byte {
	r.mu.Lock()
	seed := slices.Clone(r.keySeed)
	r.mu.Unlock()
	key, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		return nil
	}
	return key
}

// activationCarriers is the carrier list the loop offers: what is bound now.
// With tcp missing the loop holds every post, so the role says so itself.
func (r *extenderRole) activationCarriers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	carriers := make([]string, 0, len(r.carriers))
	for carrier, up := range r.carriers {
		if up {
			carriers = append(carriers, carrier)
		}
	}
	slices.Sort(carriers)
	return carriers
}

func (r *extenderRole) activationDnsPorts() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.dnsPorts)
}

// StartActivation attaches the activation loop for one direct slot. Calling it
// again for the same slot is a no-op; for a new slot it replaces the old loop.
func (r *extenderRole) StartActivation(inputs *extenderActivationInputs) {
	// Attach, detach and replacement are one sequence: two of them interleaving
	// could each read "no activator", each build one, and leave the loser
	// running with nothing holding it. The lock is never taken by the loop (its
	// closures take r.mu only), so closing a loop while holding it cannot deadlock.
	r.activationMu.Lock()
	defer r.activationMu.Unlock()
	// A slot that has already ended gets no activation: its token is frozen and
	// nothing would ever stop the loop short of the role closing.
	if inputs.ctx.Err() != nil {
		return
	}
	r.mu.Lock()
	if r.activator != nil && r.activatorInputs == inputs {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	// Keep the old loop alive while a replacement waits for the platform.
	r.primeRootKeys(inputs, extenderRootKeysPrimeTimeout)
	if inputs.ctx.Err() != nil {
		return
	}
	r.mu.Lock()
	previous := r.activator
	r.activator = nil
	r.activatorInputs = nil
	r.mu.Unlock()
	// closing joins the loop, whose carrier closures take r.mu: never hold it
	if previous != nil {
		previous.Close()
	}

	activator := connect.NewExtenderActivator(r.ctx, r.activatorSettings(inputs))

	r.mu.Lock()
	if r.ctx.Err() != nil {
		r.mu.Unlock()
		activator.Close()
		return
	}
	r.activator = activator
	r.activatorInputs = inputs
	r.mu.Unlock()
	// reads the platform's root keys for as long as this slot lives
	go connect.HandleError(func() { r.loadRootKeys(inputs) }, r.cancel)
	importantLogf("🌉✅ [extender] activation started: this host now asks the platform to list it for IPv4 and IPv6. Each family is tried separately; results are logged as they happen.\n")
	r.publishStatus("activation started")
}

// activationOwnedBy reports whether the running activation is the one built for
// `inputs`.
func (r *extenderRole) activationOwnedBy(inputs *extenderActivationInputs) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activator != nil && r.activatorInputs == inputs
}

// StopActivation ends the loop that belongs to `inputs`, if it is the current
// one. A loop for a newer slot is left alone.
func (r *extenderRole) StopActivation(inputs *extenderActivationInputs) {
	r.activationMu.Lock()
	defer r.activationMu.Unlock()
	r.mu.Lock()
	if r.activator == nil || r.activatorInputs != inputs {
		r.mu.Unlock()
		return
	}
	activator := r.activator
	r.activator = nil
	r.activatorInputs = nil
	r.mu.Unlock()
	activator.Close()
	importantLogf("🌉 [extender] activation paused: this provider lost its connection to the platform, so it stops asking to be listed for now. It starts asking again by itself as soon as the connection is back.\n")
}

// currentActivator is the running loop, nil before the direct slot attaches.
func (r *extenderRole) currentActivator() *connect.ExtenderActivator {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activator
}

// newExtenderActivationStrategy is the strategy every activation and hello is
// posted through. It is built fresh and never shares the slot's strategy,
// because the slot's defaults carry extender inputs (networks, hostnames, a
// maximum count) that could route the post through another extender. The
// platform publishes the address it sees, so a post that crossed an extender
// would list the wrong host.
func newExtenderActivationStrategy(ctx context.Context) *connect.ClientStrategy {
	return connect.NewClientStrategy(ctx, extenderActivationStrategySettings())
}

// extenderActivationStrategySettings is the direct-only settings, apart from
// the strategy so a test can pin that no extender input is left in them.
func extenderActivationStrategySettings() *connect.ClientStrategySettings {
	settings := connect.DefaultClientStrategySettings()
	settings.ExtenderNetworks = nil
	settings.ExtenderHostnames = nil
	settings.MaxExtenderCount = 0
	settings.ExpandExtenderProfileCount = 0
	settings.DisableManualExtenders = true
	settings.DisableVless = true
	// The platform's probe can take close to its own 10 s budget, and a busy
	// answer can queue, so the post must be allowed at least the activator's own
	// 60 s request budget. The default 15 s transport timeout cut a legitimate
	// activation short, after the platform had already spent the attempt.
	settings.ConnectSettings.RequestTimeout = 90 * time.Second
	return settings
}
