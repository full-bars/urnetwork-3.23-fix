package urnettools

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// containerMarkerPath is the file Docker creates at the root of every
// container. Overridable so tests can simulate running inside one.
var containerMarkerPath = "/.dockerenv"

// inContainer reports whether this tool is running inside a Docker
// container, where the provider has no systemd unit and is supervised by the
// image's start script instead.
func inContainer() bool {
	_, err := os.Stat(containerMarkerPath)
	return err == nil
}

// pelicanMode reports whether the container runs under the Pelican panel
// (PELICAN=yes), which owns the image and forbids swapping the provider
// binary at runtime.
func pelicanMode() bool {
	return strings.EqualFold(os.Getenv("PELICAN"), "yes")
}

// errPelicanUpdatesDisabled refuses runtime binary updates under Pelican. The
// panel pins the image, so the only supported update is pulling a new one.
var errPelicanUpdatesDisabled = errors.New("runtime updates are disabled under Pelican: update by re-pulling the image")

// refuseInContainer rejects systemd lifecycle verbs inside a container. The
// start script relaunches the provider whenever it exits, so stop cannot keep
// it stopped and start has nothing to do; the container itself is the unit.
func refuseInContainer(verb string) error {
	if !inContainer() {
		return nil
	}
	return fmt.Errorf("%s is not available inside the container: the image's start script supervises the provider. From the host, run `docker %s <container>` or `urnet-docker %s <container>`", verb, verb, verb)
}

// hubContainerRefusal returns the guidance for hub subcommands that cannot
// run from inside a provider container, or nil when sub is allowed there.
func hubContainerRefusal(sub string) error {
	switch sub {
	case "update", "install":
		return errors.New("in Docker, update the hub by pulling a new image:\n  docker pull ghcr.io/full-bars/urnetwork-3.23-fix-hub:latest\nor re-create the container with the updated image")
	case "init", "onboard-cmd", "show-password":
		return errors.New("hub-side commands (init, onboard-cmd, show-password) run inside the hub container:\n  docker exec <hub-container> /hub -mint-onboard-token -data /data\n  docker exec <hub-container> /hub -show-password -data /data")
	}
	return nil
}
