package main

import (
	"context"
	"errors"
	"net"
	"time"
)

// proxyIPv6UDPExit asks a pre-resolved IPv6 STUN server through an existing
// SOCKS relay. Only an IPv6 mapped address from the expected server is accepted.
func proxyIPv6UDPExit(ctx context.Context, pc net.PacketConn, destination *net.UDPAddr) (string, error) {
	if destination == nil || destination.IP == nil || destination.IP.To4() != nil || destination.IP.To16() == nil || destination.Port < 1 || destination.Port > 65535 {
		return "", errors.New("invalid IPv6 STUN destination")
	}
	buffer := make([]byte, 2048)
	request, transaction := newStunBindingRequest()
	for attempt := 0; attempt < udpCheckStunAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if _, err := pc.WriteTo(request, destination); err != nil {
			return "", err
		}
		deadline := time.Now().Add(udpCheckReplyTimeout)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		if err := pc.SetReadDeadline(deadline); err != nil {
			return "", err
		}
		for {
			n, source, err := pc.ReadFrom(buffer)
			if err != nil {
				break
			}
			peer, ok := source.(*net.UDPAddr)
			if !ok || peer.Port != destination.Port || !peer.IP.Equal(destination.IP) {
				continue
			}
			if ip, ok := parseStunMappedAddress(buffer[:n], transaction); ok && ip.To4() == nil {
				return ip.String(), nil
			}
		}
	}
	return "", errors.New("the relay did not return an IPv6 STUN address")
}

func proxyIPv6UDPStatus(tcpExit, udpExit string) string {
	tcp := net.ParseIP(tcpExit)
	udp := net.ParseIP(udpExit)
	if tcp == nil || tcp.To4() != nil || udp == nil || udp.To4() != nil {
		return "unknown"
	}
	if !tcp.Equal(udp) {
		return "mismatch"
	}
	return "yes"
}
