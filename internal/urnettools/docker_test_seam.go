package urnettools

// Package-level helper used by _test.go files to override the docker binary
// path without trusting the $URNET_DOCKER_BIN environment variable in
// production. Production code never calls this function, so the var stays
// empty under normal builds.
func setDockerTestBin(bin string) {
	testDockerBin = bin
}
