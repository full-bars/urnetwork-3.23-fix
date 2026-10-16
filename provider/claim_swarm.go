package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/urnetwork/connect/internal/durablepath"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const ClaimSwarmSchema = "urnetwork-claim-swarm-v1"

type ClaimSwarmMember struct {
	ID         string `json:"id"`
	ConfigPath string `json:"config_path"`
}

type ClaimSwarmConfig struct {
	Schema        string             `json:"schema"`
	ListenAddress string             `json:"listen_address"`
	Members       []ClaimSwarmMember `json:"members"`
}

func (self ClaimSwarmConfig) Validate() error {
	if self.Schema != ClaimSwarmSchema || len(self.Members) == 0 || len(self.Members) > 1_000 {
		return errors.New("claim swarm requires schema v1 and between 1 and 1000 members")
	}
	listen, err := netip.ParseAddrPort(self.ListenAddress)
	if err != nil || !listen.Addr().IsLoopback() || listen.Port() == 0 {
		return errors.New("claim swarm status listener must be a nonzero loopback address")
	}
	seenIDs := map[string]bool{}
	seenConfigs := map[string]bool{}
	for index, member := range self.Members {
		if member.ID == "" || len(member.ID) > 128 || seenIDs[member.ID] || strings.ContainsAny(member.ID, `/\`) {
			return fmt.Errorf("claim member %d has an empty, duplicate or unsafe id", index)
		}
		seenIDs[member.ID] = true
		if !filepath.IsAbs(member.ConfigPath) || seenConfigs[filepath.Clean(member.ConfigPath)] {
			return fmt.Errorf("claim member %s config_path must be absolute and unique", member.ID)
		}
		seenConfigs[filepath.Clean(member.ConfigPath)] = true
	}
	return nil
}

func LoadClaimSwarmConfig(path string) (*ClaimSwarmConfig, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	decoder := json.NewDecoder(bufio.NewReader(f))
	decoder.DisallowUnknownFields()
	var config ClaimSwarmConfig
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode claim swarm: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("claim swarm config contains multiple JSON values")
		}
		return nil, fmt.Errorf("decode claim swarm trailing data: %w", err)
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &config, nil
}

type claimSwarmStatus struct {
	Schema     string            `json:"schema"`
	Configured int               `json:"configured"`
	Running    int               `json:"running"`
	Failures   map[string]string `json:"failures,omitempty"`
}

type ClaimSwarm struct {
	config    *ClaimSwarmConfig
	stateLock sync.Mutex
	running   map[string]bool
	failures  map[string]string
	progress  map[string]*claimProgressOwner
}

func NewClaimSwarm(config *ClaimSwarmConfig) (*ClaimSwarm, error) {
	if config == nil {
		return nil, errors.New("claim swarm config is nil")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	progress := map[string]*claimProgressOwner{}
	for _, member := range config.Members {
		progress[member.ID] = nil
	}
	return &ClaimSwarm{config: config, running: map[string]bool{}, failures: map[string]string{}, progress: progress}, nil
}

func (self *ClaimSwarm) status() claimSwarmStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	failures := map[string]string{}
	for id, detail := range self.failures {
		failures[id] = detail
	}
	return claimSwarmStatus{Schema: ClaimSwarmSchema, Configured: len(self.config.Members), Running: len(self.running), Failures: failures}
}

func (self *ClaimSwarm) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/claim-progress" {
		self.serveClaimProgress(writer, request)
		return
	}
	if request.Method != http.MethodGet || request.URL.Path != "/status" {
		http.NotFound(writer, request)
		return
	}
	status := self.status()
	writer.Header().Set("Content-Type", "application/json")
	if status.Running != status.Configured || len(status.Failures) != 0 {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(writer).Encode(status)
}

func loadClaimSwarmMembers(config *ClaimSwarmConfig) (map[string]*ClaimDaemonConfig, time.Duration, error) {
	loaded := make(map[string]*ClaimDaemonConfig, len(config.Members))
	stateOwnerKVs := map[string]string{}
	var keyFile string
	var rpc []string
	minimumPoll := time.Duration(0)
	for _, member := range config.Members {
		claimConfig, err := LoadClaimDaemonConfig(member.ConfigPath)
		if err != nil {
			return nil, 0, fmt.Errorf("claim member %s: %w", member.ID, err)
		}
		if claimConfig.JWTFile == "" {
			return nil, 0, fmt.Errorf("claim member %s must bind an explicit jwt_file", member.ID)
		}
		stateDir, err := canonicalClaimStateDirectory(claimConfig.StateDir)
		if err != nil {
			return nil, 0, fmt.Errorf("claim member %s state directory: %w", member.ID, err)
		}
		if prior := stateOwnerKVs[stateDir]; prior != "" {
			return nil, 0, fmt.Errorf("claim members %s and %s share one queue state directory", prior, member.ID)
		}
		stateOwnerKVs[stateDir] = member.ID
		if keyFile == "" {
			keyFile = claimConfig.KeyFile
			rpc = append([]string(nil), claimConfig.RPC...)
		} else if claimConfig.KeyFile != keyFile || !slices.Equal(claimConfig.RPC, rpc) {
			return nil, 0, errors.New("claim swarm members must share one relayer key and exact RPC failover list")
		}
		poll := time.Duration(claimConfig.PollSeconds) * time.Second
		if minimumPoll == 0 || poll < minimumPoll {
			minimumPoll = poll
		}
		loaded[member.ID] = claimConfig
	}
	return loaded, minimumPoll, nil
}

func (self *ClaimSwarm) Run(ctx context.Context) (runErr error) {
	return self.run(ctx, nil)
}

// The optional observer runs after a joined member result. It cannot replace
// admission, worker execution, publication or any stored evidence.
func (self *ClaimSwarm) run(ctx context.Context, afterMember func(string, error, context.Context)) (runErr error) {
	if ctx == nil {
		return errors.New("claim swarm context is nil")
	}
	if err := durablepath.Require(ctx); err != nil {
		return err
	}
	loaded, pollPeriod, err := loadClaimSwarmMembers(self.config)
	if err != nil {
		return err
	}
	for _, member := range self.config.Members {
		owner := newClaimProgressOwner(member.ID, loaded[member.ID].ProgressPool)
		loaded[member.ID].progress = owner
		self.stateLock.Lock()
		self.progress[member.ID] = owner
		self.stateLock.Unlock()
	}
	defer func() {
		for _, member := range self.config.Members {
			loaded[member.ID].progress.close()
		}
	}()
	members := append([]ClaimSwarmMember(nil), self.config.Members...)
	sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })
	stores := make(map[string]*claimQueueStore, len(members))
	defer func() {
		for _, member := range members {
			runErr = errors.Join(runErr, stores[member.ID].close())
		}
	}()
	// Acquire every queue before reading custody or starting any sibling.
	// Partial admission releases only our descriptors and leaves bytes untouched.
	for _, member := range members {
		store, err := newClaimQueueStore(loaded[member.ID].StateDir, ctx)
		if err != nil {
			return fmt.Errorf("claim member %s queue ownership: %w", member.ID, err)
		}
		stores[member.ID] = store
	}
	admission := &claimAdmission{}
	// Seed the complete nonce domain before even the first member can sign.
	// External signers using this key require a separate shared nonce owner.
	for _, member := range members {
		if err := admission.seedMember(loaded[member.ID], stores[member.ID]); err != nil {
			return fmt.Errorf("seed relayer nonce custody: %w", err)
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{Addr: self.config.ListenAddress, Handler: self}
	serverErrors := make(chan error, 1)
	go func() {
		err := server.ListenAndServe()
		if !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
	}()
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	type memberResult struct {
		id  string
		err error
	}
	terminalErrors := make(chan memberResult, len(members))
	var membersDone sync.WaitGroup
	defer func() { cancel(); membersDone.Wait() }()
	for index, member := range members {
		delay := time.Duration(index) * pollPeriod / time.Duration(len(members))
		membersDone.Add(1)
		go func(member ClaimSwarmMember, initialDelay time.Duration) {
			defer membersDone.Done()
			defer loaded[member.ID].progress.close()
			onReady := func() {
				self.stateLock.Lock()
				self.running[member.ID] = true
				delete(self.failures, member.ID)
				self.stateLock.Unlock()
			}
			onFailure := func(err error) {
				loaded[member.ID].progress.unavailable()
				self.stateLock.Lock()
				delete(self.running, member.ID)
				self.failures[member.ID] = err.Error()
				self.stateLock.Unlock()
			}
			runErr := runClaimOwner(runCtx, loaded[member.ID], stores[member.ID], claimOwnerHooks{
				run: func(owner *claimQueueStore) error {
					return runClaimDaemonWithStore(runCtx, loaded[member.ID], owner, admission, initialDelay, onReady)
				}, state: onFailure,
			})
			if runErr != nil {
				onFailure(runErr)
				runErr = fmt.Errorf("claim member %s: %w", member.ID, runErr)
			}
			terminalErrors <- memberResult{id: member.ID, err: runErr}
		}(member, delay)
	}
	// A stopped member remains visible as unhealthy while its independent
	// siblings retain their owners. Shared nonce custody still gates every
	// signature through admission; a member error never resets that domain.
	var failures []error
	for remaining := len(members); remaining > 0; remaining-- {
		select {
		case <-ctx.Done():
			return nil
		case err := <-serverErrors:
			return err
		case terminal := <-terminalErrors:
			failures = append(failures, terminal.err)
			if afterMember != nil {
				afterMember(terminal.id, terminal.err, runCtx)
			}
		}
	}
	return errors.Join(failures...)
}

func RunClaimSwarm(ctx context.Context, configPath string) error {
	if err := durablepath.Require(ctx); err != nil {
		return err
	}
	config, err := LoadClaimSwarmConfig(configPath)
	if err != nil {
		return err
	}
	swarm, err := NewClaimSwarm(config)
	if err != nil {
		return err
	}
	return swarm.Run(ctx)
}
