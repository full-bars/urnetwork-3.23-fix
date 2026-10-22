package extender

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// extender_seam.go -- the disabled seams of the slices that have not landed
// yet.
//
// The server core of this package (extender.go, extender_cert.go,
// extender_conn.go, extender_handler.go, extender_proxy.go,
// extender_admission.go, extender_dns.go, extender_datagram.go and
// extender_probe.go) is wired for three features that are ported in later
// slices: the tcp camouflage (extender_camouflage.go), the NLayer relay
// (extender_nlayer.go, extender_nlayer_probe.go) and the lag.dev measurement
// API (extender_lag_*.go). This file carries the smallest definitions those
// wiring points need so the core builds, tests and serves as the plain
// forwarder: camouflage is off (no static key), NLayer never has a hop, and the
// measurement API never serves. Each later slice deletes its section here when
// the real file lands, so nothing in this file is intended to survive the
// port.

// --- camouflage (EXTENDER.md P) ---

// The tcp camouflage of the core build: off, so no ClientHello is peeked and
// the connection goes straight to the extender's own tls server.
type extenderCamouflage struct {
	server *ExtenderServer
}

func newExtenderCamouflage(server *ExtenderServer, settings *ExtenderSettings) (*extenderCamouflage, error) {
	return nil, nil
}

func (self *extenderCamouflage) StaticPublicKey() []byte {
	return nil
}

func (self *extenderCamouflage) close() {}

// camouflageDemultiplex is the peek in front of tls.Server. Without the
// camouflage there is nothing to classify, so every connection is terminated
// by the extender as the unauthenticated path does.
func (self *ExtenderServer) camouflageDemultiplex(ctx context.Context, conn net.Conn) (net.Conn, bool, bool) {
	return conn, false, false
}

// --- NLayer (EXTENDER.md A11) ---

// The remote extender one hop of an NLayer chain names, with its hold state.
// The core build has no hops.
type extenderNLayerHop struct{}

// The identity a relayed probe is keyed by.
type nlayerProbeSource struct{}

func newExtenderNLayerHops(extenderConfigs []*connect.ExtenderConfig) []*extenderNLayerHop {
	return nil
}

func (self *ExtenderServer) newNLayerConnectSettings() *connect.ConnectSettings {
	return nil
}

// The depth bound (A11) applies to every extender, with or without hops: a
// request whose HopCount says this extender would be deeper than the chain
// bound is refused, which is what ends a chain that loops.
func (self *ExtenderServer) checkNLayerDepth(header *protocol.ExtenderHeader) error {
	depth := int64(header.HopCount) + 1
	if maxDepth := self.settings.NLayerMaxDepth; 0 < maxDepth && int64(maxDepth) < depth {
		return errors.New("the request would be one extender too deep")
	}
	if header.HopCount == ^uint32(0) {
		return errors.New("the request has crossed too many extenders")
	}
	return nil
}

// No hop is configured, so no hop can be limited.
func (self *ExtenderServer) nlayerHopsLimited() (time.Duration, bool) {
	return 0, false
}

// serveNLayer is the relay of an accepted forward of an NLayer extender. The
// core build has no hops, so the handler and the v1 path never reach it.
func (self *ExtenderServer) serveNLayer(
	ctx context.Context,
	cancel context.CancelFunc,
	clientConn net.Conn,
	clientAddress string,
	header *protocol.ExtenderHeader,
) {
	self.reportError("nlayer", errors.New("the NLayer relay is not built in this phase"))
}

// serveNLayerProbe relays a probe of an NLayer extender to the end of its
// chain. The core build has no hops, so the handler never reaches it.
func (self *extenderHandler) serveNLayerProbe(
	w http.ResponseWriter,
	req *http.Request,
	header *protocol.ExtenderHeader,
) {
	self.refuse(w, req, "probe", errors.New("the NLayer relay is not built in this phase"))
}

// --- lag.dev measurement API ---

// lagCertificates is what the API presents: the measurement names served now
// and their certificate.
type lagCertificates interface {
	Certificate() (names []string, certificate *tls.Certificate)
}

// lagReport is the finished session's report as the core build sees it. The
// lag slice brings the real protocol.LagReport message.
type lagReport struct{}

// lagReports is where a finished session's report goes.
type lagReports interface {
	Submit(report *lagReport)
}

// ExtenderMeasurementCert is the measurement key and certificate the role
// keeps (lag.dev C4). The core build keeps none.
type ExtenderMeasurementCert struct{}

func (self *ExtenderMeasurementCert) Zone() string {
	return ""
}

func (self *ExtenderMeasurementCert) ChangeMonitor() *connect.MonitorValue[uint64] {
	return nil
}

// ExtenderLagSettings is the `measurement:` settings block of the API
// (lag.dev/PLAN.md M2, M3). The core build never serves, so the values only
// have to compile; the lag slice replaces this with the real API.
type ExtenderLagSettings struct {
	Enabled          bool
	AllowedOrigins   []string
	MaxSessions      int
	ConcurrentLoads  int
	DailyBudgetBytes int64
	RootPublicKeys   func() []ed25519.PublicKey
	NetworkHosts     []string
}

func DefaultExtenderLagSettings() ExtenderLagSettings {
	return ExtenderLagSettings{
		Enabled:         true,
		AllowedOrigins:  []string{"https://lag.dev", "https://ur.io"},
		MaxSessions:     512,
		ConcurrentLoads: 2,
	}
}

// ExtenderLagLifecycleSettings is the role's settings of the lifecycle that
// keeps the measurement certificate, queues its reports and serves the feed
// (lag.dev/PLAN.md M8, M10).
type ExtenderLagLifecycleSettings struct {
	Certificate *ExtenderMeasurementCert

	ApiUrl         string
	ByJwt          func() string
	ClientStrategy *connect.ClientStrategy
	RequestTimeout time.Duration

	CertPollMinBackoff   time.Duration
	CertPollMaxBackoff   time.Duration
	CertOfferPollTimeout time.Duration
	CertCheckTimeout     time.Duration

	ReportMaxBatchCount   int
	ReportMaxPendingCount int
	ReportFlushTimeout    time.Duration
	ReportMinBackoff      time.Duration
	ReportMaxBackoff      time.Duration

	Now   func() time.Time
	After func(wait time.Duration) <-chan time.Time
}

func DefaultExtenderLagLifecycleSettings() *ExtenderLagLifecycleSettings {
	return &ExtenderLagLifecycleSettings{
		RequestTimeout: 30 * time.Second,

		CertPollMinBackoff:   30 * time.Second,
		CertPollMaxBackoff:   30 * time.Minute,
		CertOfferPollTimeout: 15 * time.Second,
		CertCheckTimeout:     1 * time.Hour,

		ReportMaxBatchCount:   32,
		ReportMaxPendingCount: 256,
		ReportFlushTimeout:    10 * time.Second,
		ReportMinBackoff:      15 * time.Second,
		ReportMaxBackoff:      15 * time.Minute,
	}
}

// The lifecycle side of M10. The core build keeps no certificate, drops every
// report and serves no feed.
type extenderLagLifecycle struct {
	server *ExtenderServer
}

func newExtenderLagLifecycle(server *ExtenderServer, settings *ExtenderLagLifecycleSettings) *extenderLagLifecycle {
	return &extenderLagLifecycle{server: server}
}

func (self *extenderLagLifecycle) Certificate() (names []string, certificate *tls.Certificate) {
	return nil, nil
}

func (self *extenderLagLifecycle) Submit(report *lagReport) {}

func (self *extenderLagLifecycle) FeedHandler() http.Handler {
	return nil
}

// The zone of measurement names (N1): the lifecycle's when it keeps a
// certificate, and otherwise the default the fork names.
func lagApiMeasurementZone(lifecycleSettings *ExtenderLagLifecycleSettings) string {
	return ""
}

// The measurement API of one extender (M1 to M7). The core build never serves
// one; the lag slice brings the real API and its lifecycle.
type extenderLagApi struct {
	server *ExtenderServer
}

func newExtenderLagApi(
	server *ExtenderServer,
	lagSettings ExtenderLagSettings,
	certificates lagCertificates,
	reports lagReports,
	feedHandler http.Handler,
	certificateChanges *connect.MonitorValue[uint64],
) *extenderLagApi {
	return nil
}

func (self *extenderLagApi) close() {}

func (self *extenderLagApi) servesName(serverName string) bool {
	return false
}

// carrierCertificate selects the outer certificate of a carrier handshake
// (B3). Without the measurement API every name takes the identity leaf of the
// certificate authority.
func (self *ExtenderServer) carrierCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	return self.certificates.GetCertificate(hello)
}

// measurementName reports whether a server name is one of the measurement
// names (M1). The core build serves none.
func (self *ExtenderServer) measurementName(serverName string) bool {
	return false
}
