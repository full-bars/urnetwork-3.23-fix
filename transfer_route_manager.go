package connect

import (
	"context"
	"errors"
	"fmt"
	mathrand "math/rand"
	"reflect"
	"slices"
	"sync"
	"time"
	// "runtime/debug"

	"golang.org/x/exp/maps"
)

// manage multiple routes to a destination, allowing weighted reads and writes to the routes
// this assumes the source is a single client

// routes are expected to have flow control and error detection and rejection
type Route = chan []byte

const TransportMaxPriority = 0
const TransportMinPriority = 100
const TransportMaxWeight = float32(1)
const TransportMinWeight = float32(0)

// each transport must have a unique local id
// This solves an issue where some transports can be implemented with zero state.
// Zero state transports makes it ambiguous whether the transport pointer can be used as a key.
// see https://github.com/golang/go/issues/65878
type Transport interface {
	TransportId() Id

	// lower priority takes precedence
	Priority() int

	// the intrinsic weight of the transport, [0, 1]
	// if the transport has no preference, use 0
	Weight() float32

	CanEvalRouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) bool
	// returns the fraction of route weight that should be allocated to this transport
	// the remaining are the lower priority transports
	// call `rematchTransport` to re-evaluate the weights. this is used for a control loop where the weight is adjusted to match the actual distribution
	RouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) float32

	MatchesSend(destination TransferPath) bool
	MatchesReceive(destination TransferPath) bool

	// request that p2p and direct connections be re-established that include the source
	// connections will be denied for sources that have bad audits
	Downgrade(source TransferPath)
}

type MultiRouteWriter interface {
	Write(ctx context.Context, transferFrameBytes []byte, timeout time.Duration) error
	WriteDetailed(ctx context.Context, transferFrameBytes []byte, timeout time.Duration) (bool, error)
	GetActiveRoutes() []Route
	GetInactiveRoutes() []Route
}

type MultiRouteReader interface {
	Read(ctx context.Context, timeout time.Duration) ([]byte, error)
	GetActiveRoutes() []Route
	GetInactiveRoutes() []Route
}

type RouteManager struct {
	ctx context.Context

	clientTag string
	log       Logger

	mutex            sync.Mutex
	writerMatchState *MatchState
	readerMatchState *MatchState
}

func NewRouteManager(ctx context.Context, clientTag string) *RouteManager {
	return NewRouteManagerWithLogger(ctx, clientTag, nil)
}

func NewRouteManagerWithLogger(ctx context.Context, clientTag string, log Logger) *RouteManager {
	log = loggerOrDefault(log)
	return &RouteManager{
		ctx:              ctx,
		clientTag:        clientTag,
		log:              log,
		writerMatchState: NewMatchState(ctx, clientTag, log, true, Transport.MatchesSend),
		// `weightedRoutes=false` because unless there is a cpu limit this is not needed
		readerMatchState: NewMatchState(ctx, clientTag, log, false, Transport.MatchesReceive),
	}
}

// DowngradeReceiverConnection asks every transport registered in the reader
// match state to re-establish its connections to the source. Transports deny
// connections from sources with bad audits (see Transport.Downgrade). Holds
// the RouteManager mutex for the duration.
func (self *RouteManager) DowngradeReceiverConnection(source TransferPath) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.readerMatchState.Downgrade(source)
}

// OpenMultiRouteWriter opens a multi-route writer to a destination. The
// destination must be a destination mask (zero SourceId) — a specific
// destination id and optionally a stream id, never a specific source;
// otherwise the call panics. Each Write is delivered to one of the routes of
// the transports that match the destination. Pair every opened writer with
// CloseMultiRouteWriter. Registration is serialized by the RouteManager mutex.
func (self *RouteManager) OpenMultiRouteWriter(destination TransferPath) MultiRouteWriter {
	if !destination.IsDestinationMask() {
		panic(fmt.Errorf("Destination required for writer: %s", destination))
	}

	self.mutex.Lock()
	defer self.mutex.Unlock()

	return MultiRouteWriter(self.writerMatchState.openMultiRouteSelector(destination))
}

// CloseMultiRouteWriter unregisters the writer's selector from the writer
// match state, so it stops receiving transport route updates. The route
// channels belong to the transports and are not closed here, and the
// selector's context is not cancelled: a frame in flight to a route channel
// is unaffected. The selector must be the concrete type returned by
// OpenMultiRouteWriter. Holds the RouteManager mutex.
func (self *RouteManager) CloseMultiRouteWriter(w MultiRouteWriter) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.writerMatchState.closeMultiRouteSelector(w.(*MultiRouteSelector))
}

// OpenMultiRouteReader opens a multi-route reader to a destination. The
// destination must be a destination mask (zero SourceId); otherwise the call
// panics. Each Read pulls from the routes of the transports that match the
// destination. Pair every opened reader with CloseMultiRouteReader.
// Registration is serialized by the RouteManager mutex.
func (self *RouteManager) OpenMultiRouteReader(destination TransferPath) MultiRouteReader {
	if !destination.IsDestinationMask() {
		panic(fmt.Errorf("Destination required for reader: %s", destination))
	}

	self.mutex.Lock()
	defer self.mutex.Unlock()

	return MultiRouteReader(self.readerMatchState.openMultiRouteSelector(destination))
}

// CloseMultiRouteReader unregisters the reader's selector from the reader
// match state, so it stops receiving transport route updates. Unread frames
// remain in the transport-owned route channels; the channels are not closed
// here and the selector's context is not cancelled. The selector must be the
// concrete type returned by OpenMultiRouteReader. Holds the RouteManager
// mutex.
func (self *RouteManager) CloseMultiRouteReader(r MultiRouteReader) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.readerMatchState.closeMultiRouteSelector(r.(*MultiRouteSelector))
}

// UpdateTransport registers or replaces the routes a transport provides, in
// both the writer and reader match states. Transports call this when their
// connections are established; a nil or empty route list removes the
// transport (see RemoveTransport). Every open selector is re-matched: routes
// are wired to the destinations the transport matches, routes no longer
// present are dropped, and newly added routes start active. Safe for
// concurrent use; the RouteManager mutex serializes updates.
func (self *RouteManager) UpdateTransport(transport Transport, routes []Route) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.writerMatchState.updateTransport(transport, routes)
	self.readerMatchState.updateTransport(transport, routes)
}

// RemoveTransport removes a transport from both match states: its routes are
// dropped from every open selector and its matched-destination bookkeeping is
// deleted. Equivalent to UpdateTransport(transport, nil).
func (self *RouteManager) RemoveTransport(transport Transport) {
	self.UpdateTransport(transport, nil)
}

// getTransportStats returns the transport's accumulated send/receive counters
// across all open selectors, split by match state. A nil value for a side
// means the transport is not registered on that side. Holds the RouteManager
// mutex while aggregating.
func (self *RouteManager) getTransportStats(transport Transport) (writerStats *RouteStats, readerStats *RouteStats) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	writerStats = self.writerMatchState.getTransportStats(transport)
	readerStats = self.readerMatchState.getTransportStats(transport)
	return
}

type MatchState struct {
	ctx       context.Context
	clientTag string
	log       Logger

	weightedRoutes bool
	matches        func(Transport, TransferPath) bool

	transportRoutes map[Transport][]Route

	// destination -> multi route selectors
	destinationMultiRouteSelectors map[TransferPath]map[*MultiRouteSelector]bool

	// transport -> destinations
	transportMatchedDestinations map[Transport]map[TransferPath]bool
}

// note weighted routes typically are used by the sender not receiver
func NewMatchState(ctx context.Context, clientTag string, log Logger, weightedRoutes bool, matches func(Transport, TransferPath) bool) *MatchState {
	return &MatchState{
		ctx:                            ctx,
		clientTag:                      clientTag,
		log:                            log,
		weightedRoutes:                 weightedRoutes,
		matches:                        matches,
		transportRoutes:                map[Transport][]Route{},
		destinationMultiRouteSelectors: map[TransferPath]map[*MultiRouteSelector]bool{},
		transportMatchedDestinations:   map[Transport]map[TransferPath]bool{},
	}
}

// getTransportStats sums the per-route stats of the selectors the transport
// matches, across all destinations it currently matches. Returns nil if the
// transport is matched to no destination. Called under the RouteManager mutex.
func (self *MatchState) getTransportStats(transport Transport) *RouteStats {
	destinations, ok := self.transportMatchedDestinations[transport]
	if !ok {
		return nil
	}
	netStats := NewRouteStats()
	for destination, _ := range destinations {
		if multiRouteSelectors, ok := self.destinationMultiRouteSelectors[destination]; ok {
			for multiRouteSelector, _ := range multiRouteSelectors {
				if stats := multiRouteSelector.getTransportStats(transport); stats != nil {
					netStats.sendCount += stats.sendCount
					netStats.sendByteCount += stats.sendByteCount
					netStats.receiveCount += stats.receiveCount
					netStats.receiveByteCount += stats.receiveByteCount
				}
			}
		}
	}
	return netStats
}

// openMultiRouteSelector creates a selector for the destination, registers it
// in the destination's selector set, and wires in every transport that
// currently matches the destination, with the transport's current routes.
// Called by OpenMultiRouteWriter/OpenMultiRouteReader under the RouteManager
// mutex.
func (self *MatchState) openMultiRouteSelector(destination TransferPath) *MultiRouteSelector {
	multiRouteSelector := NewMultiRouteSelector(self.ctx, self.clientTag, self.log, destination, self.weightedRoutes)

	multiRouteSelectors, ok := self.destinationMultiRouteSelectors[destination]
	if !ok {
		multiRouteSelectors = map[*MultiRouteSelector]bool{}
		self.destinationMultiRouteSelectors[destination] = multiRouteSelectors
	}
	multiRouteSelectors[multiRouteSelector] = true

	for transport, routes := range self.transportRoutes {
		matchedDestinations, ok := self.transportMatchedDestinations[transport]
		if !ok {
			matchedDestinations := map[TransferPath]bool{}
			self.transportMatchedDestinations[transport] = matchedDestinations
		}

		// use the latest matches state
		if self.matches(transport, destination) {
			matchedDestinations[destination] = true
			multiRouteSelector.updateTransport(transport, routes)
		}
	}

	return multiRouteSelector
}

// closeMultiRouteSelector unregisters the selector from its destination's
// selector set. When the last selector for a destination is closed, the
// destination's selector set is deleted and the destination is stripped from
// every transport's matched-destination set. The selector's own state and the
// transport-owned route channels are left untouched.
func (self *MatchState) closeMultiRouteSelector(multiRouteSelector *MultiRouteSelector) {
	// TODO readers do not need to prioritize routes

	destination := multiRouteSelector.destination
	multiRouteSelectors, ok := self.destinationMultiRouteSelectors[destination]
	if !ok {
		// not present
		return
	}
	delete(multiRouteSelectors, multiRouteSelector)

	if len(multiRouteSelectors) == 0 {
		// clean up the destination
		delete(self.destinationMultiRouteSelectors, destination)
		for _, matchedDestinations := range self.transportMatchedDestinations {
			delete(matchedDestinations, destination)
		}
	}
}

// updateTransport applies a route change for a transport across every open
// selector: destinations newly matched by the transport receive its routes,
// destinations that no longer match drop them, and an empty route list
// removes the transport entirely. It maintains the transport's
// matched-destination set and the transportRoutes map, and each affected
// selector notifies its monitor so blocked reads/writes re-match. Called under
// the RouteManager mutex.
func (self *MatchState) updateTransport(transport Transport, routes []Route) {
	if len(routes) == 0 {
		if currentMatchedDestinations, ok := self.transportMatchedDestinations[transport]; ok {
			for destination, _ := range currentMatchedDestinations {
				if multiRouteSelectors, ok := self.destinationMultiRouteSelectors[destination]; ok {
					for multiRouteSelector, _ := range multiRouteSelectors {
						multiRouteSelector.updateTransport(transport, nil)
					}
				}
			}
		}

		delete(self.transportMatchedDestinations, transport)
		delete(self.transportRoutes, transport)
	} else {
		matchedDestinations := map[TransferPath]bool{}

		currentMatchedDestinations, ok := self.transportMatchedDestinations[transport]
		if !ok {
			currentMatchedDestinations = map[TransferPath]bool{}
		}

		for destination, multiRouteSelectors := range self.destinationMultiRouteSelectors {
			if self.matches(transport, destination) {
				matchedDestinations[destination] = true
				for multiRouteSelector, _ := range multiRouteSelectors {
					multiRouteSelector.updateTransport(transport, routes)
				}
			} else if _, ok := currentMatchedDestinations[destination]; ok {
				// no longer matches
				for multiRouteSelector, _ := range multiRouteSelectors {
					multiRouteSelector.updateTransport(transport, nil)
				}
			}
		}

		self.transportMatchedDestinations[transport] = matchedDestinations
		self.transportRoutes[transport] = routes
	}
}

// Downgrade asks every registered transport to re-establish its connections
// that include the source; transports deny connections from sources with bad
// audits (see Transport.Downgrade). Called under the RouteManager mutex.
func (self *MatchState) Downgrade(source TransferPath) {
	for transport, _ := range self.transportRoutes {
		transport.Downgrade(source)
	}
}

type MultiRouteSelector struct {
	ctx       context.Context
	cancel    context.CancelFunc
	clientTag string
	log       Logger

	destination    TransferPath
	weightedRoutes bool

	transportUpdate *Monitor

	mutex           sync.Mutex
	transportRoutes map[Transport][]Route
	routeStats      map[Route]*RouteStats
	routeActive     map[Route]bool
	routeWeight     map[Route]float32
}

func NewMultiRouteSelector(ctx context.Context, clientTag string, log Logger, destination TransferPath, weightedRoutes bool) *MultiRouteSelector {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &MultiRouteSelector{
		ctx:             cancelCtx,
		cancel:          cancel,
		clientTag:       clientTag,
		log:             loggerOrDefault(log),
		destination:     destination,
		weightedRoutes:  weightedRoutes,
		transportUpdate: NewMonitor(),
		transportRoutes: map[Transport][]Route{},
		routeStats:      map[Route]*RouteStats{},
		routeActive:     map[Route]bool{},
		routeWeight:     map[Route]float32{},
	}
}

// getTransportStats sums the send/receive counters of the routes currently
// registered to the transport within this selector. Returns nil if the
// transport is not registered with the selector. Holds the selector mutex.
func (self *MultiRouteSelector) getTransportStats(transport Transport) *RouteStats {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	currentRoutes, ok := self.transportRoutes[transport]
	if !ok {
		return nil
	}
	netStats := NewRouteStats()
	for _, currentRoute := range currentRoutes {
		if stats, ok := self.routeStats[currentRoute]; ok {
			netStats.sendCount += stats.sendCount
			netStats.sendByteCount += stats.sendByteCount
			netStats.receiveCount += stats.receiveCount
			netStats.receiveByteCount += stats.receiveByteCount
		}
	}
	return netStats
}

// if weightedRoutes, this applies new priorities and weights. calling this resets all route stats.
// the reason to reset weightedRoutes is that the weight calculation needs to consider only the stats since the previous weight change
func (self *MultiRouteSelector) updateTransport(transport Transport, routes []Route) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// activeRoutes := func()([]Route) {
	//  activeRoutes := []Route{}
	//  for _, routes := range self.transportRoutes {
	//      for _, route := range routes {
	//          if self.routeActive[route] {
	//              activeRoutes = append(activeRoutes, route)
	//          }
	//      }
	//  }
	//  return activeRoutes
	// }

	// preTransportCount := len(self.transportRoutes)
	// preActiveRouteCount := len(activeRoutes())

	if len(routes) == 0 {
		if currentRoutes, ok := self.transportRoutes[transport]; ok {
			for _, currentRoute := range currentRoutes {
				delete(self.routeStats, currentRoute)
				delete(self.routeActive, currentRoute)
				delete(self.routeWeight, currentRoute)
			}
			delete(self.transportRoutes, transport)
		} else {
			// transport is not active. nothing to do
			return
		}
	} else {
		if currentRoutes, ok := self.transportRoutes[transport]; ok {
			for _, currentRoute := range currentRoutes {
				if slices.Index(routes, currentRoute) < 0 {
					// no longer present
					delete(self.routeStats, currentRoute)
					delete(self.routeActive, currentRoute)
					delete(self.routeWeight, currentRoute)
				}
			}
			for _, route := range routes {
				if slices.Index(currentRoutes, route) < 0 {
					// new route
					self.routeActive[route] = true
				}
			}
		} else {
			for _, route := range routes {
				// new route
				self.routeActive[route] = true
			}
		}
		// the following will be updated with the new routes in the weighting below
		// - routeStats
		// - routeActive
		// - routeWeights
		self.transportRoutes[transport] = routes
	}

	if self.weightedRoutes {
		self.updateRouteWeights()
	}

	self.transportUpdate.NotifyAll()
}

// updateRouteWeights recomputes the per-route weights and resets every route
// stat, so the next computation sees only the traffic since this change.
// Transports are ordered by priority (equal priorities shuffled); each
// transport's routes receive its RouteWeight share of the remaining weight.
// Every transport must pass CanEvalRouteWeight, otherwise the weights and
// stats are left as they were. Called by updateTransport on weighted
// selectors while holding the selector mutex.
func (self *MultiRouteSelector) updateRouteWeights() {
	updatedRouteWeight := map[Route]float32{}

	transportStats := map[Transport]*RouteStats{}
	for transport, currentRoutes := range self.transportRoutes {
		netStats := NewRouteStats()
		for _, currentRoute := range currentRoutes {
			if stats, ok := self.routeStats[currentRoute]; ok {
				netStats.sendCount += stats.sendCount
				netStats.sendByteCount += stats.sendByteCount
				netStats.receiveCount += stats.receiveCount
				netStats.receiveByteCount += stats.receiveByteCount
			}
		}
		transportStats[transport] = netStats
	}

	orderedTransports := maps.Keys(self.transportRoutes)
	// shuffle the same priority values
	mathrand.Shuffle(len(orderedTransports), func(i int, j int) {
		t := orderedTransports[i]
		orderedTransports[i] = orderedTransports[j]
		orderedTransports[j] = t
	})
	slices.SortStableFunc(orderedTransports, func(a Transport, b Transport) int {
		return a.Priority() - b.Priority()
	})

	n := len(orderedTransports)

	allCanEval := true
	for i := 0; i < n; i += 1 {
		transport := orderedTransports[i]
		routeStats := transportStats[transport]
		remainingStats := map[Transport]*RouteStats{}
		for j := i + 1; j < n; j += 1 {
			remainingStats[orderedTransports[j]] = transportStats[orderedTransports[j]]
		}
		canEval := transport.CanEvalRouteWeight(routeStats, remainingStats)
		allCanEval = allCanEval && canEval
	}

	if allCanEval {
		var allWeight float32
		allWeight = 1.0
		for i := 0; i < n; i += 1 {
			transport := orderedTransports[i]
			routeStats := transportStats[transport]
			remainingStats := map[Transport]*RouteStats{}
			for j := i + 1; j < n; j += 1 {
				remainingStats[orderedTransports[j]] = transportStats[orderedTransports[j]]
			}
			weight := transport.RouteWeight(routeStats, remainingStats)
			for _, route := range self.transportRoutes[transport] {
				updatedRouteWeight[route] = allWeight * weight
			}
			allWeight *= (1.0 - weight)
		}

		self.routeWeight = updatedRouteWeight

		updatedRouteStats := map[Route]*RouteStats{}
		for _, currentRoutes := range self.transportRoutes {
			for _, currentRoute := range currentRoutes {
				// reset the stats
				updatedRouteStats[currentRoute] = NewRouteStats()
			}
		}
		self.routeStats = updatedRouteStats
	}
}

// GetActiveRoutes returns a new slice of the routes that are currently
// active, shuffled so a caller can pick one at random: a weighted shuffle
// (favoring higher route weights) on a weighted selector, otherwise a plain
// shuffle. A route is active from the moment it is added by updateTransport
// until a read observes its channel closed. Holds the selector mutex.
func (self *MultiRouteSelector) GetActiveRoutes() []Route {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	activeRoutes := []Route{}
	for _, routes := range self.transportRoutes {
		for _, route := range routes {
			if self.routeActive[route] {
				activeRoutes = append(activeRoutes, route)
			}
		}
	}

	if self.weightedRoutes {
		// prioritize the routes (weighted shuffle)
		// if all weights are equal, this is the same as a shuffle
		WeightedShuffle(activeRoutes, self.routeWeight)
	} else {
		mathrand.Shuffle(len(activeRoutes), func(i int, j int) {
			activeRoutes[i], activeRoutes[j] = activeRoutes[j], activeRoutes[i]
		})
	}

	return activeRoutes
}

// GetInactiveRoutes returns a new slice of the registered routes that are not
// currently active — routes whose channel a read observed closed, for
// example. Not shuffled. Holds the selector mutex.
func (self *MultiRouteSelector) GetInactiveRoutes() []Route {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	inactiveRoutes := []Route{}
	for _, routes := range self.transportRoutes {
		for _, route := range routes {
			if !self.routeActive[route] {
				inactiveRoutes = append(inactiveRoutes, route)
			}
		}
	}

	return inactiveRoutes
}

// setActive records whether a route is eligible for read/write selection. A
// read marks a route inactive when it observes the route's channel closed.
// Holds the selector mutex.
func (self *MultiRouteSelector) setActive(route Route, active bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.routeActive[route] = active
}

// updateSendStats adds sendCount/sendByteCount to the route's stats, creating
// the entry on first use. Called once per frame delivered by WriteDetailed.
// The counters feed transport-level stats aggregation (getTransportStats) and,
// on weighted selectors, the next weight recomputation. Holds the selector
// mutex.
func (self *MultiRouteSelector) updateSendStats(route Route, sendCount int, sendByteCount ByteCount) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	stats, ok := self.routeStats[route]
	if !ok {
		stats = NewRouteStats()
		self.routeStats[route] = stats
	}
	stats.sendCount += sendCount
	stats.sendByteCount += sendByteCount
}

// updateReceiveStats adds receiveCount/receiveByteCount to the route's stats,
// creating the entry on first use. Called once per frame delivered by Read.
// The counters feed transport-level stats aggregation (getTransportStats) and,
// on weighted selectors, the next weight recomputation. Holds the selector
// mutex.
func (self *MultiRouteSelector) updateReceiveStats(route Route, receiveCount int, receiveByteCount ByteCount) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	stats, ok := self.routeStats[route]
	if !ok {
		stats = NewRouteStats()
		self.routeStats[route] = stats
	}
	stats.receiveCount += receiveCount
	stats.receiveByteCount += receiveByteCount
}

// Write delivers transferFrameBytes whole to exactly one of the currently
// active routes and returns nil; partial writes are not possible. If no route
// can take the frame within the timeout it returns a "Timeout." error; a
// cancelled caller or selector context propagates as the WriteDetailed error
// ("Context done"/"Done"). On any failure the frame has already been returned
// to the message pool, so the caller must treat it as consumed.
func (self *MultiRouteSelector) Write(ctx context.Context, transferFrameBytes []byte, timeout time.Duration) error {
	success, err := self.WriteDetailed(ctx, transferFrameBytes, timeout)
	if err != nil {
		return err
	}
	if !success {
		return errors.New("Timeout.")
	}
	return nil
}

// MultiRouteWriter
func (self *MultiRouteSelector) WriteDetailed(ctx context.Context, transferFrameBytes []byte, timeout time.Duration) (bool, error) {
	// write to the first channel available, in random priority
	enterTime := time.Now()
	for {
		notify := self.transportUpdate.NotifyChannel()
		activeRoutes := self.GetActiveRoutes()

		self.log.V(2).Infof("[mrw] %s->%s s(%s) routes = %d\n", self.clientTag, self.destination.DestinationId, self.destination.StreamId, len(activeRoutes))

		// non-blocking priority
		for _, route := range activeRoutes {
			select {
			case route <- transferFrameBytes:
				self.log.V(2).Infof("[mrw]nb %s->%s s(%s)\n", self.clientTag, self.destination.DestinationId, self.destination.StreamId)
				self.updateSendStats(route, 1, ByteCount(len(transferFrameBytes)))
				return true, nil
			default:
			}
		}

		// select cases are in order:
		// - ctx.Done
		// - self.ctx.Done
		// - route writes...
		// - transport update
		// - timeout (may not exist)

		selectCases := make([]reflect.SelectCase, 0, 4+len(activeRoutes))

		// add the context done case
		contextDoneIndex := len(selectCases)
		selectCases = append(selectCases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(ctx.Done()),
		})

		// add the done case
		doneIndex := len(selectCases)
		selectCases = append(selectCases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(self.ctx.Done()),
		})

		// add the update case
		transportUpdateIndex := len(selectCases)
		selectCases = append(selectCases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(notify),
		})

		// add all the route
		routeStartIndex := len(selectCases)
		if 0 < len(activeRoutes) {
			sendValue := reflect.ValueOf(transferFrameBytes)
			for _, route := range activeRoutes {
				selectCases = append(selectCases, reflect.SelectCase{
					Dir:  reflect.SelectSend,
					Chan: reflect.ValueOf(route),
					Send: sendValue,
				})
			}
		}

		timeoutIndex := len(selectCases)
		if 0 <= timeout {
			remainingTimeout := enterTime.Add(timeout).Sub(time.Now())
			if remainingTimeout <= 0 {
				// add a default case
				selectCases = append(selectCases, reflect.SelectCase{
					Dir: reflect.SelectDefault,
				})
			} else {
				// add a timeout case
				selectCases = append(selectCases, reflect.SelectCase{
					Dir:  reflect.SelectRecv,
					Chan: reflect.ValueOf(time.After(remainingTimeout)),
				})
			}
		}

		if chosenIndex, _, _ := reflect.Select(selectCases); 0 <= chosenIndex {
			self.log.V(2).Infof("[mrw]b %s->%s s(%s)\n", self.clientTag, self.destination.DestinationId, self.destination.SourceId)

			switch chosenIndex {
			case contextDoneIndex:
				MessagePoolReturn(transferFrameBytes)
				return false, errors.New("Context done")
			case doneIndex:
				MessagePoolReturn(transferFrameBytes)
				return false, errors.New("Done")
			case transportUpdateIndex:
				// new routes, try again
			case timeoutIndex:
				MessagePoolReturn(transferFrameBytes)
				return false, nil
			default:
				// a route
				routeIndex := chosenIndex - routeStartIndex
				route := activeRoutes[routeIndex]
				self.updateSendStats(route, 1, ByteCount(len(transferFrameBytes)))
				return true, nil
			}
		}
	}
}

// MultiRouteReader
func (self *MultiRouteSelector) Read(ctx context.Context, timeout time.Duration) ([]byte, error) {
	// read from the first channel available, in random priority
	enterTime := time.Now()
	for {
		notify := self.transportUpdate.NotifyChannel()
		activeRoutes := self.GetActiveRoutes()

		self.log.V(2).Infof("[mrr] %s/%s<- s(%s) routes = %d\n", self.clientTag, self.destination.DestinationId, self.destination.StreamId, len(activeRoutes))

		// non-blocking priority
		retry := false
		for _, route := range activeRoutes {
			select {
			case transferFrameBytes, ok := <-route:
				if ok {
					self.log.V(2).Infof("[mrr]nb %s/%s<- s(%s)\n", self.clientTag, self.destination.DestinationId, self.destination.StreamId)
					self.updateReceiveStats(route, 1, ByteCount(len(transferFrameBytes)))
					return transferFrameBytes, nil
				} else {
					// mark the route as closed, try again
					self.setActive(route, false)
					retry = true
				}
			default:
			}
		}
		if retry {
			continue
		}

		// select cases are in order:
		// - ctx.Done
		// - self.ctx.Done
		// - route reads...
		// - transport update
		// - timeout (may not exist)

		selectCases := make([]reflect.SelectCase, 0, 4+len(activeRoutes))

		// add the context done case
		contextDoneIndex := len(selectCases)
		selectCases = append(selectCases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(ctx.Done()),
		})

		// add the done case
		doneIndex := len(selectCases)
		selectCases = append(selectCases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(self.ctx.Done()),
		})

		// add the update case
		transportUpdateIndex := len(selectCases)
		selectCases = append(selectCases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(notify),
		})

		// add all the route
		routeStartIndex := len(selectCases)
		if 0 < len(activeRoutes) {
			for _, route := range activeRoutes {
				selectCases = append(selectCases, reflect.SelectCase{
					Dir:  reflect.SelectRecv,
					Chan: reflect.ValueOf(route),
				})
			}
		}

		timeoutIndex := len(selectCases)
		if 0 <= timeout {
			remainingTimeout := enterTime.Add(timeout).Sub(time.Now())
			if remainingTimeout <= 0 {
				// add a default case
				selectCases = append(selectCases, reflect.SelectCase{
					Dir: reflect.SelectDefault,
				})
			} else {
				// add a timeout case
				selectCases = append(selectCases, reflect.SelectCase{
					Dir:  reflect.SelectRecv,
					Chan: reflect.ValueOf(time.After(remainingTimeout)),
				})
			}
		}

		chosenIndex, value, ok := reflect.Select(selectCases)
		self.log.V(2).Infof("[mrr]b %s/%s<- s(%s)\n", self.clientTag, self.destination.DestinationId, self.destination.StreamId)

		switch chosenIndex {
		case contextDoneIndex:
			return nil, errors.New("Context done")
		case doneIndex:
			return nil, errors.New("Done")
		case transportUpdateIndex:
			// new routes, try again
		case timeoutIndex:
			// FIXME return nil, nil? don't use errors for timeouts
			return nil, nil
		default:
			// a route
			routeIndex := chosenIndex - routeStartIndex
			route := activeRoutes[routeIndex]
			if ok {
				transferFrameBytes := value.Bytes()
				self.updateReceiveStats(route, 1, ByteCount(len(transferFrameBytes)))
				return transferFrameBytes, nil
			} else {
				// mark the route as closed, try again
				self.setActive(route, false)
			}
		}
	}
}

// Close cancels the selector's context, so a blocked Read or Write returns
// immediately with a "Done" error. It does not close the route channels (they
// are owned by the transports) and does not unregister the selector from its
// match state.
func (self *MultiRouteSelector) Close() {
	self.cancel()
}

type RouteStats struct {
	sendCount        int
	sendByteCount    ByteCount
	receiveCount     int
	receiveByteCount ByteCount
}

func NewRouteStats() *RouteStats {
	return &RouteStats{
		sendCount:        0,
		sendByteCount:    ByteCount(0),
		receiveCount:     0,
		receiveByteCount: ByteCount(0),
	}
}

// conforms to `Transport`
type sendClientTransport struct {
	transportId  Id
	complement   bool
	destinations map[TransferPath]bool
}

func NewSendClientTransport(destinations ...TransferPath) *sendClientTransport {
	return NewSendClientTransportWithComplement(false, destinations...)
}

func NewSendClientTransportWithComplement(complement bool, destinations ...TransferPath) *sendClientTransport {
	destinations_ := map[TransferPath]bool{}
	for _, destination := range destinations {
		destinations_[destination] = true
	}
	return &sendClientTransport{
		transportId:  NewId(),
		complement:   complement,
		destinations: destinations_,
	}
}

// TransportId returns the transport's unique id; every instance is assigned a
// fresh id at construction.
func (self *sendClientTransport) TransportId() Id {
	return self.transportId
}

// Priority returns the fixed minimum priority (TransportMinPriority, 100).
// Lower priority values take precedence during route weighting.
func (self *sendClientTransport) Priority() int {
	return 100
}

// Weight returns the intrinsic weight 0, meaning this transport has no
// preference and the routing weight is assigned uniformly.
func (self *sendClientTransport) Weight() float32 {
	return 0
}

// CanEvalRouteWeight reports that the route weight is always evaluable; it
// returns true unconditionally.
func (self *sendClientTransport) CanEvalRouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) bool {
	return true
}

// RouteWeight returns a uniform share of the remaining weight, so transports
// of equal priority are used equally.
func (self *sendClientTransport) RouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) float32 {
	// uniform weight
	return 1.0 / float32(1+len(remainingStats))
}

// MatchesSend reports whether the destination is in the transport's
// destination set; with complement, the set is inverted and everything
// outside it matches.
func (self *sendClientTransport) MatchesSend(destination TransferPath) bool {
	return self.complement != self.destinations[destination]
}

// MatchesReceive never matches: this is a send-only transport.
func (self *sendClientTransport) MatchesReceive(destination TransferPath) bool {
	return false
}

// Downgrade is a no-op: this transport has no connection to re-establish.
func (self *sendClientTransport) Downgrade(source TransferPath) {
	// nothing to downgrade
}

// conforms to `Transport`
type sendGatewayTransport struct {
	transportId Id
}

func NewSendGatewayTransport() *sendGatewayTransport {
	return &sendGatewayTransport{
		transportId: NewId(),
	}
}

// TransportId returns the transport's unique id; every instance is assigned a
// fresh id at construction.
func (self *sendGatewayTransport) TransportId() Id {
	return self.transportId
}

// Priority returns the fixed minimum priority (TransportMinPriority, 100).
// Lower priority values take precedence during route weighting.
func (self *sendGatewayTransport) Priority() int {
	return 100
}

// Weight returns the intrinsic weight 0, meaning this transport has no
// preference and the routing weight is assigned uniformly.
func (self *sendGatewayTransport) Weight() float32 {
	return 0
}

// CanEvalRouteWeight reports that the route weight is always evaluable; it
// returns true unconditionally.
func (self *sendGatewayTransport) CanEvalRouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) bool {
	return true
}

// RouteWeight returns a uniform share of the remaining weight, so transports
// of equal priority are used equally.
func (self *sendGatewayTransport) RouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) float32 {
	// uniform weight
	return 1.0 / float32(1+len(remainingStats))
}

// MatchesSend matches every destination: the platform can route any
// destination on the send side.
func (self *sendGatewayTransport) MatchesSend(destination TransferPath) bool {
	return true
}

// MatchesReceive never matches: this is a send-only transport.
func (self *sendGatewayTransport) MatchesReceive(destination TransferPath) bool {
	return false
}

// Downgrade is a no-op: this transport has no connection to re-establish.
func (self *sendGatewayTransport) Downgrade(source TransferPath) {
	// nothing to downgrade
}

// conforms to `Transport`
type receiveGatewayTransport struct {
	transportId Id
}

func NewReceiveGatewayTransport() *receiveGatewayTransport {
	return &receiveGatewayTransport{
		transportId: NewId(),
	}
}

// TransportId returns the transport's unique id; every instance is assigned a
// fresh id at construction.
func (self *receiveGatewayTransport) TransportId() Id {
	return self.transportId
}

// Priority returns the fixed minimum priority (TransportMinPriority, 100).
// Lower priority values take precedence during route weighting.
func (self *receiveGatewayTransport) Priority() int {
	return 100
}

// Weight returns the intrinsic weight 0, meaning this transport has no
// preference and the routing weight is assigned uniformly.
func (self *receiveGatewayTransport) Weight() float32 {
	return 0
}

// CanEvalRouteWeight reports that the route weight is always evaluable; it
// returns true unconditionally.
func (self *receiveGatewayTransport) CanEvalRouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) bool {
	return true
}

// RouteWeight returns a uniform share of the remaining weight, so transports
// of equal priority are used equally.
func (self *receiveGatewayTransport) RouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) float32 {
	// uniform weight
	return 1.0 / float32(1+len(remainingStats))
}

// MatchesSend never matches: this is a receive-only transport.
func (self *receiveGatewayTransport) MatchesSend(destination TransferPath) bool {
	return false
}

// MatchesReceive matches every destination: the platform can receive from any
// destination on the receive side.
func (self *receiveGatewayTransport) MatchesReceive(destination TransferPath) bool {
	return true
}

// Downgrade is a no-op: this transport has no connection to re-establish.
func (self *receiveGatewayTransport) Downgrade(source TransferPath) {
	// nothing to downgrade
}

// conforms to `Transport`
type prioritySendGatewayTransport struct {
	transportId Id
	priority    int
	weight      float32
}

func NewPrioritySendGatewayTransport(priority int, weight float32) *prioritySendGatewayTransport {
	return &prioritySendGatewayTransport{
		transportId: NewId(),
		priority:    priority,
		weight:      weight,
	}
}

// TransportId returns the transport's unique id; every instance is assigned a
// fresh id at construction.
func (self *prioritySendGatewayTransport) TransportId() Id {
	return self.transportId
}

// Priority returns the configured priority: lower values take precedence over
// the fixed-priority (100) gateway transports.
func (self *prioritySendGatewayTransport) Priority() int {
	return self.priority
}

// Weight returns the configured intrinsic weight in [0, 1].
func (self *prioritySendGatewayTransport) Weight() float32 {
	return self.weight
}

// CanEvalRouteWeight reports that the route weight is always evaluable; it
// returns true unconditionally.
func (self *prioritySendGatewayTransport) CanEvalRouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) bool {
	return true
}

// RouteWeight returns the transport's share of the remaining weight,
// proportional to its own weight over the total of its weight and the
// remaining transports' weights; when that total is zero it falls back to a
// uniform share.
func (self *prioritySendGatewayTransport) RouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) float32 {
	netWeight := self.weight
	for t, _ := range remainingStats {
		netWeight += t.Weight()
	}
	if 0 < netWeight {
		return self.weight / netWeight
	} else {
		return 1.0 / float32(1+len(remainingStats))
	}
}

// MatchesSend matches every destination: the platform can route any
// destination on the send side.
func (self *prioritySendGatewayTransport) MatchesSend(destination TransferPath) bool {
	return true
}

// MatchesReceive never matches: this is a send-only transport.
func (self *prioritySendGatewayTransport) MatchesReceive(destination TransferPath) bool {
	return false
}

// Downgrade is a no-op: this transport has no connection to re-establish.
func (self *prioritySendGatewayTransport) Downgrade(source TransferPath) {
	// nothing to downgrade
}

// conforms to `Transport`
type priorityReceiveGatewayTransport struct {
	transportId Id
	priority    int
	weight      float32
}

func NewPriorityReceiveGatewayTransport(priority int, weight float32) *priorityReceiveGatewayTransport {
	return &priorityReceiveGatewayTransport{
		transportId: NewId(),
		priority:    priority,
		weight:      weight,
	}
}

// TransportId returns the transport's unique id; every instance is assigned a
// fresh id at construction.
func (self *priorityReceiveGatewayTransport) TransportId() Id {
	return self.transportId
}

// Priority returns the configured priority: lower values take precedence over
// the fixed-priority (100) gateway transports.
func (self *priorityReceiveGatewayTransport) Priority() int {
	return self.priority
}

// Weight returns the configured intrinsic weight in [0, 1].
func (self *priorityReceiveGatewayTransport) Weight() float32 {
	return self.weight
}

// CanEvalRouteWeight reports that the route weight is always evaluable; it
// returns true unconditionally.
func (self *priorityReceiveGatewayTransport) CanEvalRouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) bool {
	return true
}

// RouteWeight returns the transport's share of the remaining weight,
// proportional to its own weight over the total of its weight and the
// remaining transports' weights; when that total is zero it falls back to a
// uniform share.
func (self *priorityReceiveGatewayTransport) RouteWeight(stats *RouteStats, remainingStats map[Transport]*RouteStats) float32 {
	netWeight := self.weight
	for t, _ := range remainingStats {
		netWeight += t.Weight()
	}
	if 0 < netWeight {
		return self.weight / netWeight
	} else {
		return 1.0 / float32(1+len(remainingStats))
	}
}

// MatchesSend never matches: this is a receive-only transport.
func (self *priorityReceiveGatewayTransport) MatchesSend(destination TransferPath) bool {
	return false
}

// MatchesReceive matches every destination: the platform can receive from any
// destination on the receive side.
func (self *priorityReceiveGatewayTransport) MatchesReceive(destination TransferPath) bool {
	return true
}

// Downgrade is a no-op: this transport has no connection to re-establish.
func (self *priorityReceiveGatewayTransport) Downgrade(source TransferPath) {
	// nothing to downgrade
}
