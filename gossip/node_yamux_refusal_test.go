package gossip

import (
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	multistream "github.com/multiformats/go-multistream"
)

func TestGossipNodeMemoryRefusalKeepsExistingStream(t *testing.T) {
	target := newTestGossipTarget(t, func(settings *NodeSettings) {
		settings.NegotiationTimeout = testHeldNegotiationTimeout
	})
	conn := target.dial(newTestKey(t))
	healthy, err := conn.OpenStream(target.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer healthy.Close()
	if err := healthy.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	err = multistream.SelectProtoOrFail(testUnservedProtocol, healthy)
	var unsupported multistream.ErrNotSupported[string]
	if !errors.As(err, &unsupported) {
		t.Fatalf("initial negotiation: %v", err)
	}

	manager := target.node.host.Network().ResourceManager()
	limit := target.node.settings.ResourceLimits.MemoryByteCount
	err = manager.ViewSystem(func(scope network.ResourceScope) error {
		span, err := scope.BeginSpan()
		if err != nil {
			return err
		}
		t.Cleanup(span.Done)
		remaining := int(limit - scope.Stat().Memory)
		if err := span.ReserveMemory(remaining, 255); err != nil {
			return err
		}
		if err := span.ReserveMemory(1, 255); err == nil {
			return errors.New("memory limit did not reject an extra byte")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("exhaust memory: %v", err)
	}
	refused, err := conn.OpenStream(target.ctx)
	if err == nil {
		defer refused.Close()
		if err := refused.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		err = multistream.SelectProtoOrFail(testUnservedProtocol, refused)
		if !errors.Is(err, network.ErrReset) {
			t.Fatalf("extra stream was not reset: %v", err)
		}
	}
	if conn.IsClosed() {
		t.Error("connection closed after refusing stream")
	}
	message := []byte(testUnservedProtocol + "\n")
	var prefix [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(prefix[:], uint64(len(message)))
	if _, err := healthy.Write(append(prefix[:n], message...)); err != nil {
		t.Fatalf("existing stream write: %v", err)
	}
	token, err := multistream.ReadNextToken[string](healthy)
	if err != nil || token != "na" {
		t.Fatalf("existing stream read = %q, %v", token, err)
	}
	if err := manager.ViewSystem(func(scope network.ResourceScope) error {
		if scope.Stat().Memory > limit {
			return errors.New("memory limit exceeded")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
