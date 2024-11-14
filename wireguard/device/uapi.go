/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2023 WireGuard LLC. All Rights Reserved.
 */

package device

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

const (
	IpcErrorInvalid   = 1
	IpcErrorPortInUse = 2
)

type IPCError struct {
	code int64 // error code
	err  error // underlying/wrapped error
}

func (s IPCError) Error() string {
	return fmt.Sprintf("IPC error %d: %v", s.code, s.err)
}

func (s IPCError) Unwrap() error {
	return s.err
}

func (s IPCError) ErrorCode() int64 {
	return s.code
}

func ipcErrorf(code int64, msg string, args ...any) *IPCError {
	return &IPCError{code: code, err: fmt.Errorf(msg, args...)}
}

// IpcGet serializes the device and its peers into a wgtypes.Device struct.
//
// This function implements the WireGuard configuration protocol "get" operation.
// See https://www.wireguard.com/xplatform/#configuration-protocol for details.
func (device *Device) IpcGet() (*wgtypes.Device, error) {
	device.ipcMutex.RLock()
	defer device.ipcMutex.RUnlock()

	keyf := func(key *[32]byte) string {
		const hex = "0123456789abcdef"
		hexKey := make([]byte, len(key)*2)
		for i := 0; i < len(key); i++ {
			hexKey[i*2] = hex[key[i]>>4]
			hexKey[i*2+1] = hex[key[i]&0xf]
		}
		return string(hexKey)
	}

	wgDevice := wgtypes.Device{}

	// lock required resources

	device.net.RLock()
	defer device.net.RUnlock()

	device.staticIdentity.RLock()
	defer device.staticIdentity.RUnlock()

	device.peers.RLock()
	defer device.peers.RUnlock()

	// Serialize device related values //

	// private and public key

	if !device.staticIdentity.privateKey.IsZero() {
		privK, err := HexToKey(keyf((*[32]byte)(&device.staticIdentity.privateKey)))
		if err != nil {
			return nil, ipcErrorf(IpcErrorInvalid, "failed to parse private key: %w", err)
		}
		wgDevice.PrivateKey = privK
		wgDevice.PublicKey = privK.PublicKey()
	}

	// listen port

	if device.net.port != 0 {
		wgDevice.ListenPort = int(device.net.port)
	}

	// fwmark

	if device.net.fwmark != 0 {
		wgDevice.FirewallMark = int(device.net.fwmark)
	}

	for _, peer := range device.peers.keyMap {
		wgPeer := wgtypes.Peer{}

		// Serialize peer state //

		// public key

		peer.handshake.mutex.RLock()
		pubK, err := HexToKey(keyf((*[32]byte)(&peer.handshake.remoteStatic)))
		if err != nil {
			return nil, ipcErrorf(IpcErrorInvalid, "failed to parse peer public key: %w", err)
		}
		wgPeer.PublicKey = pubK

		// preshared key

		preK, err := HexToKey(keyf((*[32]byte)(&peer.handshake.presharedKey)))
		if err != nil {
			return nil, ipcErrorf(IpcErrorInvalid, "failed to parse peer preshared key: %w", err)
		}
		wgPeer.PresharedKey = preK
		peer.handshake.mutex.RUnlock()

		// protocol version

		wgPeer.ProtocolVersion = 1

		// endpoint

		peer.endpoint.Lock()
		if peer.endpoint.val != nil {
			endpoint, err := net.ResolveUDPAddr("udp", peer.endpoint.val.ToString())
			if err != nil {
				return nil, ipcErrorf(IpcErrorInvalid, "failed to parse peer endpoint: %w", err)
			}
			wgPeer.Endpoint = endpoint
		}
		peer.endpoint.Unlock()

		// last handshake time

		nano := peer.lastHandshakeNano.Load()
		secs := nano / time.Second.Nanoseconds()
		nano %= time.Second.Nanoseconds()
		wgPeer.LastHandshakeTime = time.Unix(secs, nano)

		// rx/tx bytes

		wgPeer.TransmitBytes = int64(peer.txBytes.Load())
		wgPeer.ReceiveBytes = int64(peer.rxBytes.Load())

		// persistent keepalive interval

		wgPeer.PersistentKeepaliveInterval = time.Duration(peer.persistentKeepaliveInterval.Load()) * time.Second

		// allowed ips

		device.allowedips.EntriesForPeer(peer, func(prefix netip.Prefix) bool {
			_, ipNet, err := net.ParseCIDR(prefix.String())
			if err != nil {
				device.log.Verbosef("UAPI: could not parse allowed ip of peer (skipping)%v", err)
			} else {
				wgPeer.AllowedIPs = append(wgPeer.AllowedIPs, *ipNet)
			}
			return true
		})

		wgDevice.Peers = append(wgDevice.Peers, wgPeer)
	}

	return &wgDevice, nil
}

// IpcSet deserializes a wgtypes.Config struct and applies it to the device.
//
// This function implements the WireGuard configuration protocol "set" operation.
// See https://www.wireguard.com/xplatform/#configuration-protocol for details.
func (device *Device) IpcSet(deviceConfig *wgtypes.Config) (err error) {
	device.ipcMutex.Lock()
	defer device.ipcMutex.Unlock()

	defer func() {
		if err != nil {
			device.log.Errorf("%v", err)
		}
	}()

	// Configuring Device //

	// private key

	if deviceConfig.PrivateKey != nil {
		privKey, err := KeyToHex(*deviceConfig.PrivateKey)
		if err != nil {
			return ipcErrorf(IpcErrorInvalid, "failed to parse private key: %w", err)
		}

		var sk NoisePrivateKey
		err = sk.FromMaybeZeroHex(privKey)
		if err != nil {
			return ipcErrorf(IpcErrorInvalid, "failed to set private key: %w", err)
		}
		device.log.Verbosef("UAPI: Updating private key")
		device.SetPrivateKey(sk)
	}

	// listen port

	if deviceConfig.ListenPort != nil {
		device.log.Verbosef("UAPI: Updating listen port")

		device.net.Lock()
		device.net.port = uint16(*deviceConfig.ListenPort)
		device.net.Unlock()

		// update port and rebind
		if err := device.BindUpdate(); err != nil {
			return ipcErrorf(IpcErrorPortInUse, "failed to set listen port: %w", err)
		}
	}

	// fwmark

	if deviceConfig.FirewallMark != nil {
		device.log.Verbosef("UAPI: Updating fwmark")
		if err := device.BindSetMark(uint32(*deviceConfig.FirewallMark)); err != nil {
			return ipcErrorf(IpcErrorPortInUse, "failed to update fwmark: %w", err)
		}
	}

	// replace peers

	if deviceConfig.ReplacePeers {
		device.log.Verbosef("UAPI: Removing all peers")
		device.RemoveAllPeers()
	}

	for _, peerConfig := range deviceConfig.Peers {

		// Configuring Peers //

		// public key and update only (adding peer)

		pubKey, err := KeyToHex(peerConfig.PublicKey)
		if err != nil {
			return ipcErrorf(IpcErrorInvalid, "failed to parse peer public key: %w", err)
		}

		var publicKey NoisePublicKey
		err = publicKey.FromHex(pubKey)
		if err != nil {
			return ipcErrorf(IpcErrorInvalid, "failed to get peer by public key: %w", err)
		}

		device.staticIdentity.RLock()
		if device.staticIdentity.publicKey.Equals(publicKey) {
			device.staticIdentity.RUnlock()
			continue // ignore peer with the same public key as this device
		}
		device.staticIdentity.RUnlock()

		currentPeer := device.LookupPeer(publicKey)
		created := currentPeer == nil

		if created && peerConfig.UpdateOnly {
			continue // peer is new and update only is set -> skip peer
		}

		if created {
			currentPeer, err = device.NewPeer(publicKey)
			if err != nil {
				return ipcErrorf(IpcErrorInvalid, "failed to create new peer: %w", err)
			}
			device.log.Verbosef("%v - UAPI: Created", currentPeer)
		}

		// remove peer

		if peerConfig.Remove {
			device.log.Verbosef("%v - UAPI: Removing", currentPeer)
			device.RemovePeer(currentPeer.handshake.remoteStatic)
		}

		// preshared key

		if peerConfig.PresharedKey != nil {
			preKey, err := KeyToHex(*peerConfig.PresharedKey)
			if err != nil {
				return ipcErrorf(IpcErrorInvalid, "failed to parse peer public key: %w", err)
			}

			device.log.Verbosef("%v - UAPI: Updating preshared key", currentPeer)

			currentPeer.handshake.mutex.Lock()
			err = currentPeer.handshake.presharedKey.FromHex(preKey)
			currentPeer.handshake.mutex.Unlock()

			if err != nil {
				return ipcErrorf(IpcErrorInvalid, "failed to set preshared key: %w", err)
			}
		}

		// endpoint

		if peerConfig.Endpoint != nil {
			device.log.Verbosef("%v - UAPI: Updating endpoint", currentPeer)
			endp := *peerConfig.Endpoint
			endpStr := fmt.Sprintf("%s:%d", endp.IP.String(), endp.Port)
			endpoint, err := device.net.bind.ParseEndpoint(endpStr)
			if err != nil {
				return ipcErrorf(IpcErrorInvalid, "failed to set endpoint %v: %w", endpStr, err)
			}
			currentPeer.endpoint.Lock()
			defer currentPeer.endpoint.Unlock()
			currentPeer.endpoint.val = endpoint
		}

		// persistent keepalive interval

		pkaOn := false

		if peerConfig.PersistentKeepaliveInterval != nil {
			device.log.Verbosef("%v - UAPI: Updating persistent keepalive interval", peerConfig)
			secs := peerConfig.PersistentKeepaliveInterval.Seconds()
			old := currentPeer.persistentKeepaliveInterval.Swap(uint32(secs))
			pkaOn = old == 0 && secs != 0 // send immediate keepalive if we're turning it on and before it wasn't on.
		}

		// replace allowed ips

		if peerConfig.ReplaceAllowedIPs {
			device.log.Verbosef("%v - UAPI: Removing all allowedips", currentPeer)
			device.allowedips.RemoveByPeer(currentPeer)
		}

		// allowed ips

		for _, ipNet := range peerConfig.AllowedIPs {
			device.log.Verbosef("%v - UAPI: Adding allowedip", currentPeer)
			prefix, err := netip.ParsePrefix(ipNet.String())
			if err != nil {
				return ipcErrorf(IpcErrorInvalid, "failed to set allowed ip: %w", err)
			}
			device.allowedips.Insert(prefix, currentPeer)
		}

		// post config steps

		handlePeerPostConfig(currentPeer, created, pkaOn)
	}

	return // err should be nil
}

// handlePostConfig is called after a peer configuration block has been processed.
//
// peer is the peer that was just configured.
// created reports whether the peer was created during this configuration block.
// pkaOn reports whether the peer had the persistent keepalive changed to on.
func handlePeerPostConfig(peer *Peer, created bool, pkaOn bool) {
	if peer == nil {
		return
	}
	if created {
		peer.endpoint.disableRoaming = peer.device.net.brokenRoaming && peer.endpoint.val != nil
	}
	if peer.device.isUp() {
		peer.Start()
		if pkaOn {
			peer.SendKeepalive()
		}
		peer.SendStagedPackets()
	}
}

// HexToKey converts a hex string to a wgtypes key.
func HexToKey(key string) (wgtypes.Key, error) {
	keyBytes, err := hex.DecodeString(key)
	if err != nil {
		return wgtypes.Key{}, err
	}

	wgKey, err := wgtypes.ParseKey(base64.StdEncoding.EncodeToString(keyBytes))
	if err != nil {
		return wgtypes.Key{}, err
	}
	return wgKey, nil
}

// KeyToHex converts a wgtypes key to a hex string.
func KeyToHex(key wgtypes.Key) (string, error) {
	decodedKey, err := base64.StdEncoding.DecodeString(key.String())
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(decodedKey), nil
}
