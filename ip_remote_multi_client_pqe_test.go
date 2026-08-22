package connect

import (
	"context"
	"fmt"
	"github.com/go-playground/assert/v2"
	"testing"
)

// testingEmptyMultiClientGenerator is a minimal MultiClientGenerator stub
// that yields no clients/destinations. Trimmed from upstream connect's
// ip_block_action_test.go (not ported here — unrelated block-action test
// suite) since these pqe/window-type tests only need the stub, not that
// whole file.
type testingEmptyMultiClientGenerator struct {
}

func (self *testingEmptyMultiClientGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	return map[MultiHopId]DestinationStats{}, nil
}

func (self *testingEmptyMultiClientGenerator) NewClientArgs() (*MultiClientGeneratorClientArgs, error) {
	return nil, fmt.Errorf("no clients")
}

func (self *testingEmptyMultiClientGenerator) RemoveClientArgs(args *MultiClientGeneratorClientArgs) {
}

func (self *testingEmptyMultiClientGenerator) RemoveClientWithArgs(client *Client, args *MultiClientGeneratorClientArgs) {
}

func (self *testingEmptyMultiClientGenerator) NewClientSettings() *ClientSettings {
	return DefaultClientSettings()
}

func (self *testingEmptyMultiClientGenerator) NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
	return nil, fmt.Errorf("no clients")
}

func (self *testingEmptyMultiClientGenerator) FixedDestinationSize() (int, bool) {
	return 0, false
}

// testingRecordingGenerator records the client settings a window client is
// created with, and fails the creation so no real client spins up.
type testingRecordingGenerator struct {
	testingEmptyMultiClientGenerator
	clientSettings *ClientSettings
}

func (self *testingRecordingGenerator) NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
	self.clientSettings = clientSettings
	return nil, fmt.Errorf("no clients")
}

// TestMultiClientChannelPqe verifies the profile's post-quantum encryption
// setting enables the e2e encryption sessions on the window clients, and
// stays off otherwise.
func TestMultiClientChannelPqe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	newChannelClientSettings := func(performanceProfile *PerformanceProfile) *ClientSettings {
		generator := &testingRecordingGenerator{}
		_, err := newMultiClientChannel(
			ctx,
			&multiClientChannelArgs{},
			generator,
			nil,
			nil,
			nil,
			nil,
			func() {},
			performanceProfile,
			DefaultMultiClientSettings(),
		)
		// the recording generator fails creation after settings are applied
		assert.Equal(t, true, err != nil)
		assert.Equal(t, true, generator.clientSettings != nil)
		return generator.clientSettings
	}

	// no profile: encryption stays off
	clientSettings := newChannelClientSettings(nil)
	assert.Equal(t, EncryptionModeOff, clientSettings.EncryptionSettings.Mode)

	// profile without pqe: encryption stays off
	clientSettings = newChannelClientSettings(&PerformanceProfile{
		WindowType:  WindowTypeAuto,
		AllowDirect: true,
	})
	assert.Equal(t, EncryptionModeOff, clientSettings.EncryptionSettings.Mode)

	// pqe on an auto profile fail-closes the e2e sessions
	clientSettings = newChannelClientSettings(&PerformanceProfile{
		WindowType:            WindowTypeAuto,
		PostQuantumEncryption: true,
	})
	assert.Equal(t, EncryptionModeRequired, clientSettings.EncryptionSettings.Mode)

	// pqe on a fixed profile fail-closes the e2e sessions
	clientSettings = newChannelClientSettings(&PerformanceProfile{
		WindowType:            WindowTypeSpeed,
		WindowSize:            DefaultWindowSizeSettings(),
		PostQuantumEncryption: true,
	})
	assert.Equal(t, EncryptionModeRequired, clientSettings.EncryptionSettings.Mode)
}
