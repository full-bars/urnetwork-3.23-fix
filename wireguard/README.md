# Go Implementation of [WireGuard](https://www.wireguard.com/) in userspace

This is an implementation of WireGuard fully in userspace. The repository is a fork of [wireguard-go](https://github.com/WireGuard/wireguard-go). 

Most distribution of WireGuard are implemented in kernel space and rely on interfaces to functions, userspace-wireguard, however, functions fully in userspace and is meant to be used programmatically.

## Usage
This sections describes the different components of userspace-wireguard and how to use them. Check the [Example](#example) section for a full example.

### `logger`
A `logger` provides logging for a device using two (Printf-style) functions `Verbosef` and `Errorf`.  Logging functions do not require a trailing newline in the format.

A logger can be created using the `logger.NewLogger` function. The first argument is the log level, which can be one of `logger.LogLevelSilent`, `logger.LogLevelError`, or `logger.LogLevelVerbose` (for debugging). The second argument is the prepend for the logger.

### `tun.Device`
A `tun.Device` is used to send/receive clear text packets on behalf of WireGuard peers. It is responsible for NAT management and routing from the local subnet of a WireGuard device to the internet.

The only available implementation of `tun.Device` is `tun.UserspaceTun` which is also the main contribution of this repository. The userspace tun makes use of `github.com/urnetwork/connect` to send/receive packets fully in userspace.

A `tun.Device` can be created using the `tun.CreateUserspaceTUN` function which requires a `logger` and possibly the public IPs for correct NAT. Additionally, a tun device provides access to an `events` channel which can be used to listen for events on the tun device through the functions `Events` and `AddEvent`. The possible types of events are `tun.EventUp`, `tun.EventDown`, and `tun.EventMTUUpdate`.

Other available functions include `Read`, `Write`, `MTU`, `Close`, and `BatchSize`.

### `device.Device`
A `device.Device` is used to manage a WireGuard device. It is responsible for creating and managing peers, sending/receiving packets (encrypted and plaintext) and handshakes, and managing the state of the device. Here is stored the static WireGuard identity (public and private key) of the device.

A `device.Device` can be created using the `device.NewDevice` function which requires a `tun.Device`, `logger` and `conn.Bind` as arguments. The `conn.Bind` is used to send handshakes and encrypted packets and a default implementation can be acquired using `conn.NewDefaultBind`.

A device also provides access to add events to the underlying events channel of the tun device using the `AddEvent` function. Other useful functions inlcude `Close`, `Wait`, `IpcSet`, and `IpcGet`.

### `Ipc`
The official WireGuard projects provides a [cross-platform userspace implementation](https://www.wireguard.com/xplatform/#interface) for consistency in configuration and management of WireGuard devices. A device can be configured using this interface using the `device.IpcSet` and `device.IpcGet` functions. For ease of use in stead of using textual configs we use [wgtypes](https://pkg.go.dev/golang.zx2c4.com/wireguard/wgctrl/wgtypes) objects.

To get the current configuration of a device, use the `device.IpcGet` function which returns a [`wgtypes.Device`](https://pkg.go.dev/golang.zx2c4.com/wireguard/wgctrl/wgtypes#Device) object. To set the configuration of a device, use the `device.IpcSet` function which requires a [`wgtypes.Config`](https://pkg.go.dev/golang.zx2c4.com/wireguard/wgctrl/wgtypes#Config) object as an argument.

## Example

Below is an example of a simple WireGuard server setup using userspace-wireguard. If you want to view a full example, check `EXAMPLE_SETUP.md` and `main.go` in the root directory.

```go
// debug logging
logger := logger.NewLogger(logger.LogLevelVerbose, "(userspace)") 

// tun device (with public IPv4 but no public IPv6)
utun, err := tun.CreateUserspaceTUN(logger, net.IPv4(1, 1, 1, 1), nil)
if err != nil {
    panic(err)
}

// wireguard device
device := device.NewDevice(utun, conn.NewDefaultBind(), logger)

// TODO: set your configuration for server
config := wgtypes.Config{ }
if err := device.IpcSet(&config); err != nil {
    panic(err)
}

device.AddEvent(tun.EventUp) // start up the device

// wait for program to terminate
term := make(chan os.Signal, 1)
signal.Notify(term, syscall.SIGTERM)
signal.Notify(term, os.Interrupt)
select {
case <-term:
case <-device.Wait():
}

device.Close() // clean up
```
