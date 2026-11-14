package extender

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

type serviceTestConn struct{}

func (serviceTestConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (serviceTestConn) Write(b []byte) (int, error)      { return len(b), nil }
func (serviceTestConn) Close() error                     { return nil }
func (serviceTestConn) LocalAddr() net.Addr              { return streamAddr{} }
func (serviceTestConn) RemoteAddr() net.Addr             { return streamAddr{} }
func (serviceTestConn) SetDeadline(time.Time) error      { return nil }
func (serviceTestConn) SetReadDeadline(time.Time) error  { return nil }
func (serviceTestConn) SetWriteDeadline(time.Time) error { return nil }

type serviceTestWriter struct{ *httptest.ResponseRecorder }

func (w serviceTestWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return serviceTestConn{}, bufio.NewReadWriter(bufio.NewReader(bytes.NewReader(nil)), bufio.NewWriter(io.Discard)), nil
}

func serviceTestRequest(t *testing.T, server *ExtenderServer, service uint32) *httptest.ResponseRecorder {
	t.Helper()
	data, err := proto.Marshal(&protocol.ExtenderHeader{Service: service})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://example.com/", bytes.NewReader(data))
	req.Header.Set("Content-Type", connect.ExtenderContentType)
	req.RemoteAddr = "192.0.2.1:1234"
	w := httptest.NewRecorder()
	(&extenderHandler{server: server}).ServeHTTP(serviceTestWriter{w}, req)
	return w
}

func TestServiceHandlersAttachDetachAndInitialSettings(t *testing.T) {
	for _, service := range []uint32{connect.ExtenderServiceGossip, connect.ExtenderServiceFeed} {
		t.Run(fmt.Sprint(service), func(t *testing.T) {
			settings := DefaultExtenderSettings()
			var stage, message string
			settings.ErrorHandler = func(s string, err error) { stage, message = s, err.Error() }
			server := NewExtenderServer(context.Background(), nil, nil, nil, nil, settings)
			defer server.Close()
			set := server.SetGossipConnHandler
			if service == connect.ExtenderServiceFeed {
				set = server.SetFeedConnHandler
			}
			checkRefusal := func() {
				t.Helper()
				stage, message = "", ""
				w := serviceTestRequest(t, server, service)
				if w.Code != http.StatusForbidden || w.Body.Len() != 0 || w.Header().Get("Connection") != "close" || w.Header().Get("Content-Length") != "0" || stage != "service" || message != fmt.Sprintf("service %d is not available", service) {
					t.Fatalf("refusal: status=%d body=%q headers=%v stage=%q error=%q", w.Code, w.Body.String(), w.Header(), stage, message)
				}
			}
			checkRefusal()
			called := 0
			set(func(net.Conn) { called++ })
			if w := serviceTestRequest(t, server, service); w.Code != http.StatusOK || called != 1 {
				t.Fatalf("attach: status=%d calls=%d", w.Code, called)
			}
			set(nil)
			checkRefusal()
			counter, ok := server.usage.refusals.Load("service")
			if !ok || counter.(*atomic.Uint64).Load() != 2 || server.usage.services[extenderUsageServiceIndex(service)].Load() != 3 || server.usage.served[extenderUsageServiceIndex(service)].Load() != 1 {
				t.Fatal("service request, served, or refusal counters changed")
			}
		})
	}
	settings := DefaultExtenderSettings()
	calls := 0
	settings.GossipConnHandler = func(net.Conn) { calls++ }
	settings.FeedConnHandler = func(net.Conn) { calls++ }
	server := NewExtenderServer(context.Background(), nil, nil, nil, nil, settings)
	defer server.Close()
	for _, service := range []uint32{connect.ExtenderServiceGossip, connect.ExtenderServiceFeed} {
		if w := serviceTestRequest(t, server, service); w.Code != http.StatusOK {
			t.Fatalf("initial handler: %d", w.Code)
		}
	}
	if calls != 2 {
		t.Fatalf("initial calls = %d", calls)
	}
}

func TestServiceHandlersConcurrentAdmissionsAndToggle(t *testing.T) {
	settings := DefaultExtenderSettings()
	settings.AdmissionActionsPerSubnetPerMinute = 10000
	server := NewExtenderServer(context.Background(), nil, nil, nil, nil, settings)
	defer server.Close()
	handler := func(net.Conn) {}
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-start
		for i := 0; i < 500; i++ {
			server.SetGossipConnHandler(handler)
			server.SetFeedConnHandler(handler)
			server.SetGossipConnHandler(nil)
			server.SetFeedConnHandler(nil)
		}
	}()
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for j := 0; j < 50; j++ {
				service := connect.ExtenderServiceGossip
				if j%2 != 0 {
					service = connect.ExtenderServiceFeed
				}
				w := serviceTestRequest(t, server, uint32(service))
				if w.Code != http.StatusOK && w.Code != http.StatusForbidden {
					t.Errorf("unexpected status %d", w.Code)
				}
			}
		}()
	}
	close(start)
	workers.Wait()
	server.SetGossipConnHandler(nil)
	server.SetFeedConnHandler(nil)
	if err := server.WaitServiceStreams(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestServiceHandlersDetachWaitAndConcurrentToggle(t *testing.T) {
	settings := DefaultExtenderSettings()
	settings.AdmissionActionsPerSubnetPerMinute = 1000
	server := NewExtenderServer(context.Background(), nil, nil, nil, nil, settings)
	defer server.Close()
	const n = 32
	entered := make(chan struct{}, n)
	release := make(chan struct{})
	handler := func(net.Conn) { entered <- struct{}{}; <-release }
	server.SetGossipConnHandler(handler)
	var requests sync.WaitGroup
	for i := 0; i < n; i++ {
		requests.Add(1)
		go func() { defer requests.Done(); serviceTestRequest(t, server, connect.ExtenderServiceGossip) }()
	}
	for i := 0; i < n; i++ {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("handlers did not enter")
		}
	}
	var togglers sync.WaitGroup
	for i := 0; i < 8; i++ {
		togglers.Add(1)
		go func() {
			defer togglers.Done()
			for j := 0; j < 100; j++ {
				server.SetGossipConnHandler(nil)
				server.SetGossipConnHandler(handler)
				server.SetFeedConnHandler(handler)
				server.SetFeedConnHandler(nil)
			}
		}()
	}
	togglers.Wait()
	server.SetGossipConnHandler(nil)
	server.SetFeedConnHandler(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := server.WaitServiceStreams(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- server.WaitServiceStreams(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("wait returned before release: %v", err)
	default:
	}
	close(release)
	requests.Wait()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("wait did not finish")
	}
	if err := server.WaitServiceStreams(context.Background()); err != nil {
		t.Fatal(err)
	}
}
