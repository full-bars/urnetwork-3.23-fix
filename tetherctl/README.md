# tetherctl
A way to manage wireguard devices as well as create your own without the need for a kernel module. 

***Note***: the tetherctl module looks for `bringyour.com/userspace-wireguard` at `../../userspace-wireguard` so make sure to clone `https://github.com/bringyour/userspace-wireguard` in the same folder as this repository.

## Using the package locally
When run the package allows for a cli interface where user can enter multiple command after each other. To start run:
```bash
go run . cli
```
and then use the [CLI options](#cli-options) after the provided input prompt (`>`). Check the [Example](#example) section for an example of how to use the cli.

## Using the package as a service
1. compile package and move to `/etc/tetherctl/`
```bash
go build -o tetherctl .
sudo mkdir -p /etc/tetherctl/ 
sudo mv tetherctl /etc/tetherctl/
```
2. make config file (`bywg0.conf`) and place it in `/etc/tetherctl/`
```bash
cp bywg0.conf /etc/tetherctl/bywg0.conf
```
Note: check [Config File](#config-file) for more details on how a config file is structured.

3. place service file in systemd
```bash
sudo cp by-wireguard\@.service /etc/systemd/system/
systemctl daemon-reload
```
4. enable the service to run on startup
```bash
systemctl enable by-wireguard@bywg0
```
5. start the service
```bash
systemctl start by-wireguard@bywg0 &
```
Note: *use `sudo journalctl -fu by-wireguard@bywg0` to see logs of the service or `sudo systemctl status by-wireguard@bywg0` for its status.* 

If at any point you want to stop the service you can run `systemctl stop by-wireguard@bywg0`. The service will start again if you restart your system. To fully disable it run `systemctl disable by-wireguard@bywg0`.

## CLI options
The package is meant to be used as a cli interface to manage WireGuard devices. After it is started it will wait for commands to be entered by the user. After a command is entered, the package will execute the command and print the output. After that, the package will wait for the next command. The argument `--dname` is used to specify the name of the WireGuard device that the command should be executed on. If the argument is not given, the package will use the last used value for this argument as all commands include it.

The following commands are available:

* `tetherctl --help` - prints the help menu for all commands and default values of optional arguments.
* `tetherctl --version` - prints the version of the package.
* `tetherctl add [--dname=<dname>] [--log=<log>] [--ipv4=<ipv4>] [--ipv6=<ipv6>]` - adds a wireguard device with the given name. Additionally, you can specify `<log>` for the log level (verbose, error, silent) and the public `<ipv4>` and `<ipv6>` of the server for correct NAT of packages on behalf of peers.
* `tetherctl remove [--dname=<dname>]` - removes a wireguard device with the given name.
* `tetherctl up [--dname=<dname>]  [--config=<config>]` - starts a wireguard device with the given name and applies the config file. The config file specifies the path to the folder where the config is found, i.e., for `<dname>=bywg0` and `<config>=/etc/tetherctl/` the configuration should be placed in `/etc/tetherctl/bywg0.conf`. Check [Config File](#config-file) for more details on how a config file is structured.
* `tetherctl down [--dname=<dname>] [--config=<config>] [--new_file=<new_file>]` - brings down a wireguard device with the given name and config file. The `<config>` and `<new_file>` are used to save the config of the device after it has been brought down if the option is enabled.
* `tetherctl get-config [--dname=<dname>] [--config=<config>]` - prints the config of a WireGuard device with the given name and config file. The config is given with its initial values but updated based on the peers, listen port, and keypair of the device.
* `tetherctl save-config [--dname=<dname>] [--config=<config>] [--new_file=<new_file>]` - saves the updated config (check previous bullet point) of a WireGuard device with the given name and config file. The config is saved in the given new file.
* `tetherctl gen-priv-key` - returns a private key for a WireGuard device.
* `tetherctl gen-pub-key --priv_key=<priv_key>` - returns the public key for a given private key.
* `tetherctl get-device-names` - returns the names of all available WireGuard devices.
* `tetherctl get-device [--dname=<dname>]` - returns the struct of a WireGuard device as given by `wgtypes.Device` (this includes type of device) as well as addresses of the device.
* `tetherctl change-device [--dname=<dname>] [--lport=<lport>] [--priv_key=<priv_key>]` - changes the listen port and/or private key of a WireGuard device.
* `tetherctl add-peer --pub_key=<pub_key> [--dname=<dname>] [--endpoint_type=<endpoint_type>]` - adds a peer to a WireGuard device with the given public key and endpoint (public IP of the server where it can be contacted). The endpoint type specifies which endpoint of the server the peer wants to communicate with (available values: any, ipv4, ipv6, domain). The config that the peer can use to setup its own wireguard client is returned here.
* `tetherctl remove-peer --pub_key=<pub_key> [--dname=<dname>]` - removes a peer with the given public key from a WireGuard device.
* `tetherctl get-peer-config --pub_key=<pub_key> [--dname=<dname>] [--endpoint_type=<endpoint_type>]` - returns the config of a peer with the given public key and corresponding endpoint (same config as `add-peer`).
* `tetherctl start-api [--dname=<dname>] [--api_url=<api_url>]` - starts an API server that can be used to manage the WireGuard device. The API server is started on the given URL which must include a port. Check [API](#api) for more details.
* `tetherctl stop-api` - stops the API server.

## API
To start the api use the `start-api` command. By default the API is set on `localhost:9090` but if you want to expose it to be accessible through your public ip you can set `--api_url=:9090`.

 The API has the following endpoints:
 
 * POST `/peer/add/:endpoint_type/*pubKey` - adds a peer with the given public key to the WireGuard device. The endpoint type specifies which type of endpoint the peer wants to communicate with the server on (available values: any, ipv4, ipv6, domain).  The request has no body. The config that the peer can use to setup its own wireguard client is returned here (essentially runs `add-peer` command). *Example: .../peer/add/any/this_is_a_private_key*
 * DELETE `/peer/remove/*pubKey` - removes a peer with the given public key from the WireGuard device (essentially runs `remove-peer` command). Request will succeed even if peer does not exist meaning that even if a request is accidentally repeated, the peer will only be removed once.
* GET `/peer/config/:endpoint_type/*pubKey` - returns the config of a peer with the given public key (essentially runs `get-peer-config` command) and the specified endpoint type of the server (available values: any, ipv4, ipv6, domain).

To stop the api use the `stop-api` command or just stop the program using `Ctrl+C`. Note, only one API is allowed to run at a time.

## Example 
This is an example how to use `tetherctl` to setup a device. Then, add a peer through the api allowing the peer to connect to the device.
1. Start `tetherctl` in `cli` mode
```bash
go run . cli
```
From here on, you can run the corresponding commands in the input prompt (`>`) presented by `tetherctl`.

2. Add device
```bash
add --dname=bywg0 --log=v
```

3. Start up the device
```bash
up --dname=bywg0 --config=<path-to-folder-of-config>
```
Now the device is running with the settings from the config file. An explanation of how config files are structured can be found in the [Config File](#config-file) section as well with an example config. Note, the config file must be called `bywg0.conf` for this example.

4. Start API on server
```bash
start-api --dname=bywg0 --api_url=:9090
```
5. On peer machine run `go run . cli` to start `tetherctl` and then use the following commands in the intergrated prompt (`>`) to generate a keypair.
```bash
gen-priv-key
gen-pub-key --priv_key=<private-key-from-above>
```
6. Use Postman (or any other way) to make the POST command `http://<public-ip-of-server>:9090/peer/add/any/<public-key-from-above>`
7. Get output from POST similar to:
```ini
[Interface]
PrivateKey = __PLACEHOLDER__ # replace with your private key
Address = 192.168.90.1/32
DNS = 1.1.1.1, 8.8.8.8

[Peer]
PublicKey = <public-key-of-server>
AllowedIPs = 0.0.0.0/0
Endpoint = <endpoint-of-server>
```

8. Replace the `__PLACEHOLDER__` with the key generated from step 5 and use the config in any WireGuard client to connect to the server.

9. After you are done you can stop the api using `stop-api` command or just press `Ctrl+C` to exit `tetherctl` gracefully.

## Config File
The config file is a simple `ini` formatted file similar to the ones used by [wg-quick](https://www.man7.org/linux/man-pages/man8/wg-quick.8.html) and the [wg kernel module](https://www.man7.org/linux/man-pages/man8/wg.8.html). The config file can have two sections:
* `[Interface]` - contains the configuration for the interface (mandatory, exactly one). The following options are available:
  * `Address` - a comma-separated list of IPs in CIDR notation to be assigned to the interface (can appear multiple times).
  * `ListenPort` - the port on which the interface listens.
  * `PrivateKey` - the private key of the interface (mandatory).
  * `PreUp`, `PostUp`, `PreDown`, `PostDown` - bash commands which will be executed before/after setting up/tearing down the interface (can appear multiple times). The special string `%i' is expanded to the interface name.
  * `SaveConfig` - a boolean value to save the config of the interface when being brought down. Any changes made to device while the interface is up will be saved to the config file.
* `[Peer]` - contains the configuration for a peer (optional, can appear multiple times). The following options are available:
  * `PublicKey` - the public key of the peer (mandatory).
  * `AllowedIPs` - a comma-separated list of IPs in CIDR notation that the peer is allowed to access through the interface (can appear multiple times).
  * `Endpoint` - the public IP of the server where the peer can be contacted.


### Example Config
```ini
[Interface]
Address = 192.168.90.0/24
ListenPort = 33336
PrivateKey = __PLACEHOLDER__ # replace with your private key 
SaveConfig = true

# peers will look like this
[Peer]
PublicKey = <some-peer-public-key>
AllowedIPs = 192.168.90.1/32
```