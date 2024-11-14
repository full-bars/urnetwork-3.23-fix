/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2023 WireGuard LLC. All Rights Reserved.
 */

package device

import (
	"github.com/urnetwork/connect/wireguard/tun"
)

const DefaultMTU = 1420

func (device *Device) AddEvent(event tun.Event) {
	device.tun.device.AddEvent(event)
}

func (device *Device) RoutineTUNEventReader() {
	device.log.Verbosef("Routine: event worker - started")

	for event := range device.tun.device.Events() {
		if event&tun.EventMTUUpdate != 0 {
			device.log.Verbosef("Current mtu is %v", device.tun.mtu.Load())
		}

		if event&tun.EventUp != 0 {
			device.log.Verbosef("Interface up requested")
			device.Up()
		}

		if event&tun.EventDown != 0 {
			device.log.Verbosef("Interface down requested")
			device.Down()
		}
	}

	device.log.Verbosef("Routine: event worker - stopped")
}
