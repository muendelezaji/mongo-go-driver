// Copyright (C) MongoDB, Inc. 2017-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

// Package topology contains types that handles the discovery, monitoring, and selection
// of servers. This package is designed to expose enough inner workings of service discovery
// and monitoring to allow low level applications to have fine grained control, while hiding
// most of the detailed implementation of the algorithms.
package topology // import "go.mongodb.org/mongo-driver/x/mongo/driver/topology"

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fmt"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/internal/randutil"
	"go.mongodb.org/mongo-driver/mongo/address"
	"go.mongodb.org/mongo-driver/mongo/description"
	"go.mongodb.org/mongo-driver/x/mongo/driver"
	"go.mongodb.org/mongo-driver/x/mongo/driver/dns"
)

// ErrSubscribeAfterClosed is returned when a user attempts to subscribe to a
// closed Server or Topology.
var ErrSubscribeAfterClosed = errors.New("cannot subscribe after closeConnection")

// ErrTopologyClosed is returned when a user attempts to call a method on a
// closed Topology.
var ErrTopologyClosed = errors.New("topology is closed")

// ErrTopologyConnected is returned whena  user attempts to Connect to an
// already connected Topology.
var ErrTopologyConnected = errors.New("topology is connected or connecting")

// ErrServerSelectionTimeout is returned from server selection when the server
// selection process took longer than allowed by the timeout.
var ErrServerSelectionTimeout = errors.New("server selection timeout")

// MonitorMode represents the way in which a server is monitored.
type MonitorMode uint8

// random is a package-global pseudo-random number generator.
var random = randutil.NewLockedRand(rand.NewSource(time.Now().UnixNano()))

// These constants are the available monitoring modes.
const (
	AutomaticMode MonitorMode = iota
	SingleMode
)

// Topology represents a MongoDB deployment.
type Topology struct {
	connectionstate int64

	cfg *config

	desc atomic.Value // holds a description.Topology

	dnsResolver *dns.Resolver

	done chan struct{}

	pollingRequired   bool
	pollingDone       chan struct{}
	pollingwg         sync.WaitGroup
	rescanSRVInterval time.Duration
	pollHeartbeatTime atomic.Value // holds a bool

	updateCallback updateTopologyCallback
	fsm            *fsm

	// This should really be encapsulated into it's own type. This will likely
	// require a redesign so we can share a minimum of data between the
	// subscribers and the topology.
	subscribers         map[uint64]chan description.Topology
	currentSubscriberID uint64
	subscriptionsClosed bool
	subLock             sync.Mutex

	// We should redesign how we Connect and handle individal servers. This is
	// too difficult to maintain and it's rather easy to accidentally access
	// the servers without acquiring the lock or checking if the servers are
	// closed. This lock should also be an RWMutex.
	serversLock   sync.Mutex
	serversClosed bool
	servers       map[address.Address]*Server

	id primitive.ObjectID
}

var _ driver.Deployment = &Topology{}
var _ driver.Subscriber = &Topology{}

type serverSelectionState struct {
	selector    description.ServerSelector
	timeoutChan <-chan time.Time
}

func newServerSelectionState(selector description.ServerSelector, timeoutChan <-chan time.Time) serverSelectionState {
	return serverSelectionState{
		selector:    selector,
		timeoutChan: timeoutChan,
	}
}

// New creates a new topology.
func New(opts ...Option) (*Topology, error) {
	cfg, err := newConfig(opts...)
	if err != nil {
		return nil, err
	}

	t := &Topology{
		cfg:               cfg,
		done:              make(chan struct{}),
		pollingDone:       make(chan struct{}),
		rescanSRVInterval: 60 * time.Second,
		fsm:               newFSM(),
		subscribers:       make(map[uint64]chan description.Topology),
		servers:           make(map[address.Address]*Server),
		dnsResolver:       dns.DefaultResolver,
		id:                primitive.NewObjectID(),
	}
	t.desc.Store(description.Topology{})
	t.updateCallback = func(desc description.Server) description.Server {
		return t.apply(context.TODO(), desc)
	}

	if t.cfg.uri != "" {
		t.pollingRequired = strings.HasPrefix(t.cfg.uri, "mongodb+srv://") && !t.cfg.loadBalanced
	}

	t.publishTopologyOpeningEvent()

	return t, nil
}

// Connect initializes a Topology and starts the monitoring process. This function
// must be called to properly monitor the topology.
func (t *Topology) Connect() error {
	if !atomic.CompareAndSwapInt64(&t.connectionstate, disconnected, connecting) {
		return ErrTopologyConnected
	}

	t.desc.Store(description.Topology{})
	var err error
	t.serversLock.Lock()

	// A replica set name sets the initial topology type to ReplicaSetNoPrimary unless a direct connection is also
	// specified, in which case the initial type is Single.
	if t.cfg.replicaSetName != "" {
		t.fsm.SetName = t.cfg.replicaSetName
		t.fsm.Kind = description.ReplicaSetNoPrimary
	}

	// A direct connection unconditionally sets the topology type to Single.
	if t.cfg.mode == SingleMode {
		t.fsm.Kind = description.Single
	}

	for _, a := range t.cfg.seedList {
		addr := address.Address(a).Canonicalize()
		t.fsm.Servers = append(t.fsm.Servers, description.NewDefaultServer(addr))
	}

	switch {
	case t.cfg.loadBalanced:
		// In LoadBalanced mode, we mock a series of events: TopologyDescriptionChanged from Unknown to LoadBalanced,
		// ServerDescriptionChanged from Unknown to LoadBalancer, and then TopologyDescriptionChanged to reflect the
		// previous ServerDescriptionChanged event. We publish all of these events here because we don't start server
		// monitoring routines in this mode, so we have to mock state changes.

		// Transition from Unknown with no servers to LoadBalanced with a single Unknown server.
		t.fsm.Kind = description.LoadBalanced
		t.publishTopologyDescriptionChangedEvent(description.Topology{}, t.fsm.Topology)

		addr := address.Address(t.cfg.seedList[0]).Canonicalize()
		if err := t.addServer(addr); err != nil {
			t.serversLock.Unlock()
			return err
		}

		// Transition the server from Unknown to LoadBalancer.
		newServerDesc := t.servers[addr].Description()
		t.publishServerDescriptionChangedEvent(t.fsm.Servers[0], newServerDesc)

		// Transition from LoadBalanced with an Unknown server to LoadBalanced with a LoadBalancer.
		oldDesc := t.fsm.Topology
		t.fsm.Servers = []description.Server{newServerDesc}
		t.desc.Store(t.fsm.Topology)
		t.publishTopologyDescriptionChangedEvent(oldDesc, t.fsm.Topology)
	default:
		// In non-LB mode, we only publish an initial TopologyDescriptionChanged event from Unknown with no servers to
		// the current state (e.g. Unknown with one or more servers if we're discovering or Single with one server if
		// we're connecting directly). Other events are published when state changes occur due to responses in the
		// server monitoring goroutines.

		newDesc := description.Topology{
			Kind:                  t.fsm.Kind,
			Servers:               t.fsm.Servers,
			SessionTimeoutMinutes: t.fsm.SessionTimeoutMinutes,
		}
		t.desc.Store(newDesc)
		t.publishTopologyDescriptionChangedEvent(description.Topology{}, t.fsm.Topology)
		for _, a := range t.cfg.seedList {
			addr := address.Address(a).Canonicalize()
			err = t.addServer(addr)
			if err != nil {
				t.serversLock.Unlock()
				return err
			}
		}
	}

	t.serversLock.Unlock()
	if t.pollingRequired {
		go t.pollSRVRecords()
		t.pollingwg.Add(1)
	}

	t.subscriptionsClosed = false // explicitly set in case topology was disconnected and then reconnected

	atomic.StoreInt64(&t.connectionstate, connected)
	return nil
}

// Disconnect closes the topology. It stops the monitoring thread and
// closes all open subscriptions.
func (t *Topology) Disconnect(ctx context.Context) error {
	if !atomic.CompareAndSwapInt64(&t.connectionstate, connected, disconnecting) {
		return ErrTopologyClosed
	}

	servers := make(map[address.Address]*Server)
	t.serversLock.Lock()
	t.serversClosed = true
	for addr, server := range t.servers {
		servers[addr] = server
	}
	t.serversLock.Unlock()

	for _, server := range servers {
		_ = server.Disconnect(ctx)
		t.publishServerClosedEvent(server.address)
	}

	t.subLock.Lock()
	for id, ch := range t.subscribers {
		close(ch)
		delete(t.subscribers, id)
	}
	t.subscriptionsClosed = true
	t.subLock.Unlock()

	if t.pollingRequired {
		t.pollingDone <- struct{}{}
		t.pollingwg.Wait()
	}

	t.desc.Store(description.Topology{})

	atomic.StoreInt64(&t.connectionstate, disconnected)
	t.publishTopologyClosedEvent()
	return nil
}

// Description returns a description of the topology.
func (t *Topology) Description() description.Topology {
	td, ok := t.desc.Load().(description.Topology)
	if !ok {
		td = description.Topology{}
	}
	return td
}

// Kind returns the topology kind of this Topology.
func (t *Topology) Kind() description.TopologyKind { return t.Description().Kind }

// Subscribe returns a Subscription on which all updated description.Topologys
// will be sent. The channel of the subscription will have a buffer size of one,
// and will be pre-populated with the current description.Topology.
// Subscribe implements the driver.Subscriber interface.
func (t *Topology) Subscribe() (*driver.Subscription, error) {
	if atomic.LoadInt64(&t.connectionstate) != connected {
		return nil, errors.New("cannot subscribe to Topology that is not connected")
	}
	ch := make(chan description.Topology, 1)
	td, ok := t.desc.Load().(description.Topology)
	if !ok {
		td = description.Topology{}
	}
	ch <- td

	t.subLock.Lock()
	defer t.subLock.Unlock()
	if t.subscriptionsClosed {
		return nil, ErrSubscribeAfterClosed
	}
	id := t.currentSubscriberID
	t.subscribers[id] = ch
	t.currentSubscriberID++

	return &driver.Subscription{
		Updates: ch,
		ID:      id,
	}, nil
}

// Unsubscribe unsubscribes the given subscription from the topology and closes the subscription channel.
// Unsubscribe implements the driver.Subscriber interface.
func (t *Topology) Unsubscribe(sub *driver.Subscription) error {
	t.subLock.Lock()
	defer t.subLock.Unlock()

	if t.subscriptionsClosed {
		return nil
	}

	ch, ok := t.subscribers[sub.ID]
	if !ok {
		return nil
	}

	close(ch)
	delete(t.subscribers, sub.ID)
	return nil
}

// RequestImmediateCheck will send heartbeats to all the servers in the
// topology right away, instead of waiting for the heartbeat timeout.
func (t *Topology) RequestImmediateCheck() {
	if atomic.LoadInt64(&t.connectionstate) != connected {
		return
	}
	t.serversLock.Lock()
	for _, server := range t.servers {
		server.RequestImmediateCheck()
	}
	t.serversLock.Unlock()
}

// SelectServer selects a server with given a selector. SelectServer complies with the
// server selection spec, and will time out after severSelectionTimeout or when the
// parent context is done.
func (t *Topology) SelectServer(ctx context.Context, ss description.ServerSelector) (driver.Server, error) {
	if atomic.LoadInt64(&t.connectionstate) != connected {
		return nil, ErrTopologyClosed
	}
	var ssTimeoutCh <-chan time.Time

	if t.cfg.serverSelectionTimeout > 0 {
		ssTimeout := time.NewTimer(t.cfg.serverSelectionTimeout)
		ssTimeoutCh = ssTimeout.C
		defer ssTimeout.Stop()
	}

	var doneOnce bool
	var sub *driver.Subscription
	selectionState := newServerSelectionState(ss, ssTimeoutCh)
	for {
		var suitable []description.Server
		var selectErr error

		if !doneOnce {
			// for the first pass, select a server from the current description.
			// this improves selection speed for up-to-date topology descriptions.
			suitable, selectErr = t.selectServerFromDescription(t.Description(), selectionState)
			doneOnce = true
		} else {
			// if the first pass didn't select a server, the previous description did not contain a suitable server, so
			// we subscribe to the topology and attempt to obtain a server from that subscription
			if sub == nil {
				var err error
				sub, err = t.Subscribe()
				if err != nil {
					return nil, err
				}
				defer t.Unsubscribe(sub)
			}

			suitable, selectErr = t.selectServerFromSubscription(ctx, sub.Updates, selectionState)
		}
		if selectErr != nil {
			return nil, selectErr
		}

		if len(suitable) == 0 {
			// try again if there are no servers available
			continue
		}

		selected := suitable[random.Intn(len(suitable))]
		selectedS, err := t.FindServer(selected)
		switch {
		case err != nil:
			return nil, err
		case selectedS != nil:
			return selectedS, nil
		default:
			// We don't have an actual server for the provided description.
			// This could happen for a number of reasons, including that the
			// server has since stopped being a part of this topology, or that
			// the server selector returned no suitable servers.
		}
	}
}

// FindServer will attempt to find a server that fits the given server description.
// This method will return nil, nil if a matching server could not be found.
func (t *Topology) FindServer(selected description.Server) (*SelectedServer, error) {
	if atomic.LoadInt64(&t.connectionstate) != connected {
		return nil, ErrTopologyClosed
	}
	t.serversLock.Lock()
	defer t.serversLock.Unlock()
	server, ok := t.servers[selected.Addr]
	if !ok {
		return nil, nil
	}

	desc := t.Description()
	return &SelectedServer{
		Server: server,
		Kind:   desc.Kind,
	}, nil
}

// selectServerFromSubscription loops until a topology description is available for server selection. It returns
// when the given context expires, server selection timeout is reached, or a description containing a selectable
// server is available.
func (t *Topology) selectServerFromSubscription(ctx context.Context, subscriptionCh <-chan description.Topology,
	selectionState serverSelectionState) ([]description.Server, error) {

	current := t.Description()
	for {
		select {
		case <-ctx.Done():
			return nil, ServerSelectionError{Wrapped: ctx.Err(), Desc: current}
		case <-selectionState.timeoutChan:
			return nil, ServerSelectionError{Wrapped: ErrServerSelectionTimeout, Desc: current}
		case current = <-subscriptionCh:
		}

		suitable, err := t.selectServerFromDescription(current, selectionState)
		if err != nil {
			return nil, err
		}

		if len(suitable) > 0 {
			return suitable, nil
		}
		t.RequestImmediateCheck()
	}
}

// selectServerFromDescription process the given topology description and returns a slice of suitable servers.
func (t *Topology) selectServerFromDescription(desc description.Topology,
	selectionState serverSelectionState) ([]description.Server, error) {

	// Unlike selectServerFromSubscription, this code path does not check ctx.Done or selectionState.timeoutChan because
	// selecting a server from a description is not a blocking operation.

	if desc.CompatibilityErr != nil {
		return nil, desc.CompatibilityErr
	}

	// If the topology kind is LoadBalanced, the LB is the only server and it is always considered selectable. The
	// selectors exported by the driver should already return the LB as a candidate, so this but this check ensures that
	// the LB is always selectable even if a user of the low-level driver provides a custom selector.
	if desc.Kind == description.LoadBalanced {
		return desc.Servers, nil
	}

	var allowed []description.Server
	for _, s := range desc.Servers {
		if s.Kind != description.Unknown {
			allowed = append(allowed, s)
		}
	}

	suitable, err := selectionState.selector.SelectServer(desc, allowed)
	if err != nil {
		return nil, ServerSelectionError{Wrapped: err, Desc: desc}
	}
	return suitable, nil
}

func (t *Topology) pollSRVRecords() {
	defer t.pollingwg.Done()

	serverConfig, _ := newServerConfig(t.cfg.serverOpts...)
	heartbeatInterval := serverConfig.heartbeatInterval

	pollTicker := time.NewTicker(t.rescanSRVInterval)
	defer pollTicker.Stop()
	t.pollHeartbeatTime.Store(false)
	var doneOnce bool
	defer func() {
		//  ¯\_(ツ)_/¯
		if r := recover(); r != nil && !doneOnce {
			<-t.pollingDone
		}
	}()

	// remove the scheme
	uri := t.cfg.uri[14:]
	hosts := uri
	if idx := strings.IndexAny(uri, "/?@"); idx != -1 {
		hosts = uri[:idx]
	}

	for {
		select {
		case <-pollTicker.C:
		case <-t.pollingDone:
			doneOnce = true
			return
		}
		topoKind := t.Description().Kind
		if !(topoKind == description.Unknown || topoKind == description.Sharded) {
			break
		}

		parsedHosts, err := t.dnsResolver.ParseHosts(hosts, t.cfg.srvServiceName, false)
		// DNS problem or no verified hosts returned
		if err != nil || len(parsedHosts) == 0 {
			if !t.pollHeartbeatTime.Load().(bool) {
				pollTicker.Stop()
				pollTicker = time.NewTicker(heartbeatInterval)
				t.pollHeartbeatTime.Store(true)
			}
			continue
		}
		if t.pollHeartbeatTime.Load().(bool) {
			pollTicker.Stop()
			pollTicker = time.NewTicker(t.rescanSRVInterval)
			t.pollHeartbeatTime.Store(false)
		}

		cont := t.processSRVResults(parsedHosts)
		if !cont {
			break
		}
	}
	<-t.pollingDone
	doneOnce = true
}

func (t *Topology) processSRVResults(parsedHosts []string) bool {
	t.serversLock.Lock()
	defer t.serversLock.Unlock()

	if t.serversClosed {
		return false
	}
	prev := t.fsm.Topology
	diff := diffHostList(t.fsm.Topology, parsedHosts)

	if len(diff.Added) == 0 && len(diff.Removed) == 0 {
		return true
	}

	for _, r := range diff.Removed {
		addr := address.Address(r).Canonicalize()
		s, ok := t.servers[addr]
		if !ok {
			continue
		}
		go func() {
			cancelCtx, cancel := context.WithCancel(context.Background())
			cancel()
			_ = s.Disconnect(cancelCtx)
		}()
		delete(t.servers, addr)
		t.fsm.removeServerByAddr(addr)
		t.publishServerClosedEvent(s.address)
	}

	// Now that we've removed all the hosts that disappeared from the SRV record, we need to add any
	// new hosts added to the SRV record. If adding all of the new hosts would increase the number
	// of servers past srvMaxHosts, shuffle the list of added hosts.
	if t.cfg.srvMaxHosts > 0 && len(t.servers)+len(diff.Added) > t.cfg.srvMaxHosts {
		random.Shuffle(len(diff.Added), func(i, j int) {
			diff.Added[i], diff.Added[j] = diff.Added[j], diff.Added[i]
		})
	}
	// Add all added hosts until the number of servers reaches srvMaxHosts.
	for _, a := range diff.Added {
		if t.cfg.srvMaxHosts > 0 && len(t.servers) >= t.cfg.srvMaxHosts {
			break
		}
		addr := address.Address(a).Canonicalize()
		_ = t.addServer(addr)
		t.fsm.addServer(addr)
	}

	//store new description
	newDesc := description.Topology{
		Kind:                  t.fsm.Kind,
		Servers:               t.fsm.Servers,
		SessionTimeoutMinutes: t.fsm.SessionTimeoutMinutes,
	}
	t.desc.Store(newDesc)

	if !prev.Equal(newDesc) {
		t.publishTopologyDescriptionChangedEvent(prev, newDesc)
	}

	t.subLock.Lock()
	for _, ch := range t.subscribers {
		// We drain the description if there's one in the channel
		select {
		case <-ch:
		default:
		}
		ch <- newDesc
	}
	t.subLock.Unlock()

	return true

}

// apply updates the Topology and its underlying FSM based on the provided server description and returns the server
// description that should be stored.
func (t *Topology) apply(ctx context.Context, desc description.Server) description.Server {
	t.serversLock.Lock()
	defer t.serversLock.Unlock()

	ind, ok := t.fsm.findServer(desc.Addr)
	if t.serversClosed || !ok {
		return desc
	}

	prev := t.fsm.Topology
	oldDesc := t.fsm.Servers[ind]
	if oldDesc.TopologyVersion.CompareToIncoming(desc.TopologyVersion) > 0 {
		return oldDesc
	}

	var current description.Topology
	current, desc = t.fsm.apply(desc)

	if !oldDesc.Equal(desc) {
		t.publishServerDescriptionChangedEvent(oldDesc, desc)
	}

	diff := diffTopology(prev, current)

	for _, removed := range diff.Removed {
		if s, ok := t.servers[removed.Addr]; ok {
			go func() {
				cancelCtx, cancel := context.WithCancel(ctx)
				cancel()
				_ = s.Disconnect(cancelCtx)
			}()
			delete(t.servers, removed.Addr)
			t.publishServerClosedEvent(s.address)
		}
	}

	for _, added := range diff.Added {
		_ = t.addServer(added.Addr)
	}

	t.desc.Store(current)
	if !prev.Equal(current) {
		t.publishTopologyDescriptionChangedEvent(prev, current)
	}

	t.subLock.Lock()
	for _, ch := range t.subscribers {
		// We drain the description if there's one in the channel
		select {
		case <-ch:
		default:
		}
		ch <- current
	}
	t.subLock.Unlock()

	return desc
}

func (t *Topology) addServer(addr address.Address) error {
	if _, ok := t.servers[addr]; ok {
		return nil
	}

	svr, err := ConnectServer(addr, t.updateCallback, t.id, t.cfg.serverOpts...)
	if err != nil {
		return err
	}

	t.servers[addr] = svr

	return nil
}

// String implements the Stringer interface
func (t *Topology) String() string {
	desc := t.Description()

	serversStr := ""
	t.serversLock.Lock()
	defer t.serversLock.Unlock()
	for _, s := range t.servers {
		serversStr += "{ " + s.String() + " }, "
	}
	return fmt.Sprintf("Type: %s, Servers: [%s]", desc.Kind, serversStr)
}

// publishes a ServerDescriptionChangedEvent to indicate the server description has changed
func (t *Topology) publishServerDescriptionChangedEvent(prev description.Server, current description.Server) {
	serverDescriptionChanged := &event.ServerDescriptionChangedEvent{
		Address:             current.Addr,
		TopologyID:          t.id,
		PreviousDescription: prev,
		NewDescription:      current,
	}

	if t.cfg.serverMonitor != nil && t.cfg.serverMonitor.ServerDescriptionChanged != nil {
		t.cfg.serverMonitor.ServerDescriptionChanged(serverDescriptionChanged)
	}
}

// publishes a ServerClosedEvent to indicate the server has closed
func (t *Topology) publishServerClosedEvent(addr address.Address) {
	serverClosed := &event.ServerClosedEvent{
		Address:    addr,
		TopologyID: t.id,
	}

	if t.cfg.serverMonitor != nil && t.cfg.serverMonitor.ServerClosed != nil {
		t.cfg.serverMonitor.ServerClosed(serverClosed)
	}
}

// publishes a TopologyDescriptionChangedEvent to indicate the topology description has changed
func (t *Topology) publishTopologyDescriptionChangedEvent(prev description.Topology, current description.Topology) {
	topologyDescriptionChanged := &event.TopologyDescriptionChangedEvent{
		TopologyID:          t.id,
		PreviousDescription: prev,
		NewDescription:      current,
	}

	if t.cfg.serverMonitor != nil && t.cfg.serverMonitor.TopologyDescriptionChanged != nil {
		t.cfg.serverMonitor.TopologyDescriptionChanged(topologyDescriptionChanged)
	}
}

// publishes a TopologyOpeningEvent to indicate the topology is being initialized
func (t *Topology) publishTopologyOpeningEvent() {
	topologyOpening := &event.TopologyOpeningEvent{
		TopologyID: t.id,
	}

	if t.cfg.serverMonitor != nil && t.cfg.serverMonitor.TopologyOpening != nil {
		t.cfg.serverMonitor.TopologyOpening(topologyOpening)
	}
}

// publishes a TopologyClosedEvent to indicate the topology has been closed
func (t *Topology) publishTopologyClosedEvent() {
	topologyClosed := &event.TopologyClosedEvent{
		TopologyID: t.id,
	}

	if t.cfg.serverMonitor != nil && t.cfg.serverMonitor.TopologyClosed != nil {
		t.cfg.serverMonitor.TopologyClosed(topologyClosed)
	}
}
