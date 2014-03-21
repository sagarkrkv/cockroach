// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied.  See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)

/*
Package gossip implements a protocol for sharing information between
Cockroach nodes using an ad-hoc, peer-to-peer network. The
self-assembled network aims to minimize time for new information to
reach each node, and minimize network traffic required.

Gossiped information is identified by key. Gossip information
is captured by info objects.

Groups are used to logically group related gossip values and maintain
limits on total set size. Groups organize info objects by key
prefix. An example is load and capacity characteristics for nodes. In
a cluster with thousands of nodes, groups force the gossip network to
limit itself to only a portion of total data volume.

A map of info objects and a map of Group objects are kept by an
infoStore. New info objects should be created via infoStore.newInfo.
Groups are registered via: infoStore.RegisterGroup. Info objects are
added to an infoStore using infoStore.AddInfo.

Each node attempts to contact peer nodes to gather all infos in
the system with minimal total hops. The algorithm is as follows:

 0 Node starts up gossip server to accept incoming gossip requests.
   Continue to step #1 to join the gossip network.

 1 Node selects random peer from bootstrap list, excluding its own
   address for its first outgoing connection. Node starts client and
   continues to step #2.

 2 Node requests gossip from peer. If this is first request, MaxSeq
   will be 0. Otherwise, will be value of MaxSeq from last response to
   gossip request. Requesting node times out at gossipInterval*2. On
   timeout, client is closed and GC'd. If node has no outgoing
   connections, goto #1.

   a. When gossip is received, infostore is augmented. If new info was
      received, the client in question is credited. If nothing new was
      received in maxWaitForNewGossip, client is closed. If node has no
      outgoing connections, goto #1.

   b. If any gossip was received at > maxToleratedHops and num
      connected peers < maxPeers, choose random peer from those
      originating info > maxToleratedHops, start it, and goto #2.

   c. If sentinelGossip is missing or expired, node is considered
      partitioned; goto #1.

 3 On connect, if node has too many connected clients, gossip requests
   are returned immediately with an alternate address set to a random
   selection from amongst already-connected clients. If MaxSeq is -1
   (initial connection), returns gossip immediately. Otherwise,
   request waits for a randomly jittered interval ~= gossipInterval.
   Node periodically returns empty gossip responses to prevent client
   timeouts. Node receives delta from gossiping client in turn.
*/
package gossip

import (
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"strings"
	"sync"
	"time"
)

var (
	gossipBootstrap = flag.String(
		"gossip_bootstrap", "",
		"addresses (comma-separated host:port pairs) of node addresses for gossip bootstrap")
	gossipInterval = flag.Duration(
		"gossip_interval", 2*time.Second,
		"approximate interval (time.Duration) for gossiping new information to peers")
)

const (
	// SentinelGossip is gossiped info which must not expire or node
	// considers itself partitioned and will retry with bootstrap
	// hosts.
	SentinelGossip = "meta0"
	// NodeCountGossip is the count of gossip nodes in the network.
	NodeCountGossip = "nodeCount"
	// MaxPeers is the maximum number of connected gossip peers.
	MaxPeers = 10
	// defaultNodeCount is the default number of nodes in the gossip
	// network. The actual count of nodes in the cluster is gossiped
	// by the range which contains node statistics.
	//
	// The count of nodes is used to compute the maximum hops allowed
	// for info transmission given the maxPeers parameter by the
	// formula: maxHops = ceil(log(numNodes) / log(maxPeers)) + 1.
	//
	// This default value helps when establishing the gossip network,
	// and is set purposefully high to avoid premature tightening.
	// Once we receive the gossip with actual count, the default count
	// is replaced.
	defaultNodeCount = 1000
)

// Gossip is an instance of a gossip node. It embeds a gossip server.
// During bootstrapping, the bootstrap list contains candidates for
// entre to the gossip network.
type Gossip struct {
	Name         string             // Optional node name
	*server                         // Embedded gossip RPC server
	bootstraps   *addrSet           // Bootstrap host addresses
	outgoing     *addrSet           // Set of outgoing client addresses
	clients      map[string]*client // Map from address to client
	disconnected chan *client       // Channel of disconnected clients
	exited       chan error         // Channel to signal exit
	stalled      *sync.Cond         // Indicates bootstrap is required
}

// New creates an instance of a gossip node using the specified
// node address as the gossip service endpoint.
func New(addr net.Addr) *Gossip {
	g := &Gossip{
		server:       newServer(addr, *gossipInterval),
		bootstraps:   newAddrSet(MaxPeers),
		outgoing:     newAddrSet(MaxPeers),
		clients:      make(map[string]*client),
		disconnected: make(chan *client, MaxPeers),
	}
	g.stalled = sync.NewCond(&g.mu)
	return g
}

// SetBootstrap initializes the set of gossip node addresses used to
// bootstrap the gossip network. bootstrap should be specified as a
// comma-separated list of bootstrap hosts.
func (g *Gossip) SetBootstrap(bootstraps []net.Addr) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, addr := range bootstraps {
		g.bootstraps.addAddr(addr)
	}
}

// SetInterval sets the interval at which fresh info is gossiped to
// incoming gossip clients.
func (g *Gossip) SetInterval(interval time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.interval = interval
}

// AddInt64Info adds or updates an int64-valued info. Returns nil if
// info was added; error otherwise.
func (g *Gossip) AddInt64Info(key string, val int64, ttl time.Duration) error {
	return g.addInfo(key, val, ttl)
}

// AddFloat64Info adds or updates a float64-valued info. Returns nil if
// info was added; error otherwise.
func (g *Gossip) AddFloat64Info(key string, val float64, ttl time.Duration) error {
	return g.addInfo(key, val, ttl)
}

// AddStringInfo adds or updates a string-valued info. Returns nil if
// info was added; error otherwise.
func (g *Gossip) AddStringInfo(key string, val string, ttl time.Duration) error {
	return g.addInfo(key, val, ttl)
}

// addInfo adds or updates an info by key. Returns nil if
// info was added; error otherwise.
func (g *Gossip) addInfo(key string, val interface{}, ttl time.Duration) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.is.addInfo(g.is.newInfo(key, val, ttl))
}

// GetInt64Info returns an int64 value by key or an error if specified
// key does not exist, is of another type, or has expired.
func (g *Gossip) GetInt64Info(key string) (int64, error) {
	v, err := g.getInfo(key)
	if err != nil {
		return 0, err
	}
	switch t := v.(type) {
	case int64:
		return t, nil
	default:
		return 0, fmt.Errorf("key %q is of type %s", key, t)
	}
}

// GetFloat64Info returns a float64 value by key or an error if
// specified key does not exist, is of another type, or has expired.
func (g *Gossip) GetFloat64Info(key string) (float64, error) {
	v, err := g.getInfo(key)
	if err != nil {
		return 0, err
	}
	switch t := v.(type) {
	case float64:
		return t, nil
	default:
		return 0, fmt.Errorf("key %q is of type %s", key, t)
	}
}

// GetStringInfo returns a string value by key or an error if specified
// key does not exist, is of another type, or has expired.
func (g *Gossip) GetStringInfo(key string) (string, error) {
	v, err := g.getInfo(key)
	if err != nil {
		return "", err
	}
	switch t := v.(type) {
	case string:
		return t, nil
	default:
		return "", fmt.Errorf("key %q is of type %s", key, t)
	}
}

// getInfo returns an info value by key or an error if it doesn't
// exist or has expired.
func (g *Gossip) getInfo(key string) (interface{}, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if i := g.is.getInfo(key); i != nil {
		return i.Val, nil
	}
	return nil, fmt.Errorf("key %q does not exist or has expired", key)
}

// GetGroupInt64Infos returns a slice of int64 info values from
// specified group, or an error if group is not registered.
func (g *Gossip) GetGroupInt64Infos(prefix string) ([]int64, error) {
	infos, err := g.getGroupInfos(prefix)
	if err != nil {
		return nil, err
	}
	values := make([]int64, len(infos))
	for i, info := range infos {
		switch t := info.Val.(type) {
		default:
			return nil, fmt.Errorf("value type not int64: %v", t)
		case int64:
			values[i] = t
		}
	}
	return values, nil
}

// GetGroupFloat64Infos returns a slice of float64 info values from
// specified group, or an error if group is not registered.
func (g *Gossip) GetGroupFloat64Infos(prefix string) ([]float64, error) {
	infos, err := g.getGroupInfos(prefix)
	if err != nil {
		return nil, err
	}
	values := make([]float64, len(infos))
	for i, info := range infos {
		switch t := info.Val.(type) {
		default:
			return nil, fmt.Errorf("value type not float64: %v", t)
		case float64:
			values[i] = t
		}
	}
	return values, nil
}

// GetGroupStringInfos returns a slice of string info values from
// specified group, or an error if group is not registered.
func (g *Gossip) GetGroupStringInfos(prefix string) ([]string, error) {
	infos, err := g.getGroupInfos(prefix)
	if err != nil {
		return nil, err
	}
	values := make([]string, len(infos))
	for i, info := range infos {
		switch t := info.Val.(type) {
		default:
			return nil, fmt.Errorf("value type not string: %v", t)
		case string:
			values[i] = t
		}
	}
	return values, nil
}

// getGroupInfos returns a slice of info objects from specified group,
// Returns nil if group is not registered.
func (g *Gossip) getGroupInfos(prefix string) ([]*info, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	infos := g.is.getGroupInfos(prefix)
	if infos == nil {
		return nil, fmt.Errorf("group %q doesn't exist", prefix)
	}
	return infos, nil
}

// RegisterGroup registers a new group with info store. Returns an
// error if the group was already registered.
func (g *Gossip) RegisterGroup(prefix string, limit int, typeOf GroupType) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.is.registerGroup(newGroup(prefix, limit, typeOf))
}

// MaxHops returns the maximum number of hops to reach the furthest
// gossiped information currently in the network.
func (g *Gossip) MaxHops() uint32 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.is.maxHops()
}

// Incoming returns a slice of incoming gossip client connection
// addresses.
func (g *Gossip) Incoming() []net.Addr {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.incoming.asSlice()
}

// Outgoing returns a slice of outgoing gossip client connection
// addresses. Note that these outgoing client connections may not
// actually be legitimately connected. They may be in the process
// of trying, or may already have failed, but haven't yet been
// processed by the gossip instance.
func (g *Gossip) Outgoing() []net.Addr {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.outgoing.asSlice()
}

// Start launches the gossip instance, which commences joining the
// gossip network using the node addresses supplied to NewGossip and
// specified via command-line flag: --gossip_bootstrap.
//
// This method starts bootstrap loop, gossip server, and client
// management in separate goroutines and returns.
func (g *Gossip) Start() {
	go g.serve()     // serve gossip protocol
	go g.bootstrap() // bootstrap gossip client
	go g.manage()    // manage gossip clients
}

// Close shuts down the gossip server. Returns a channel which signals
// exit once all outgoing clients are closed and the management loop
// for the gossip instance is finished.
func (g *Gossip) Close() <-chan error {
	g.stopServing()                             // set server's closed boolean and exits server
	g.stalled.Signal()                          // wake up bootstrap goroutine so it can exit
	for _, addr := range g.outgoing.asSlice() { // close all outgoing clients.
		g.closeClient(addr)
	}
	return g.exited
}

// maxToleratedHops computes the maximum number of hops which the
// gossip network should allow when optimally configured. It's based
// on the level of fanout (MaxPeers) and the count of nodes in the
// cluster.
func (g *Gossip) maxToleratedHops() uint32 {
	// Get info directly as we have mutex held here.
	var nodeCount int64 = defaultNodeCount
	if info := g.is.getInfo(NodeCountGossip); info != nil {
		nodeCount = info.Val.(int64)
	}
	return uint32(math.Ceil(math.Log(float64(nodeCount))/math.Log(float64(MaxPeers))))*2 + 1
}

// hasIncoming returns whether the server has an incoming gossip
// client matching the provided address.
func (g *Gossip) hasIncoming(addr net.Addr) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.incoming.hasAddr(addr)
}

// parseBootstrapAddresses parses the gossip bootstrap addresses
// passed via --gossip_bootstrap.
func (g *Gossip) parseBootstrapAddresses() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if *gossipBootstrap != "" {
		addresses := strings.Split(*gossipBootstrap, ",")
		for _, addr := range addresses {
			addr = strings.TrimSpace(addr)
			tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
			if err != nil {
				log.Printf("invalid gossip bootstrap address %s: %s", addr, err)
				continue
			}
			g.bootstraps.addAddr(tcpAddr)
		}
	}
	// If we have no bootstrap hosts, fatal exit.
	if g.bootstraps.len() == 0 {
		log.Fatalf("no hosts specified for gossip network (use --gossip_bootstrap)")
	}
	// Remove our own node address.
	g.bootstraps.removeAddr(g.is.NodeAddr)
}

// filterExtant removes any addresses from the supplied addrSet which
// are already connected to this node, either via outgoing or incoming
// client connection.
func (g *Gossip) filterExtant(addrs *addrSet) *addrSet {
	return addrs.filter(func(a net.Addr) bool {
		return !g.outgoing.hasAddr(a)
	}).filter(func(a net.Addr) bool {
		return !g.incoming.hasAddr(a)
	})
}

// bootstrap connects the node to the gossip network. Bootstrapping
// commences in the event there are no connected clients or the
// sentinel gossip info is not available. After a successful bootstrap
// connection, this method will block on the stalled condvar, which
// receives notifications that gossip network connectivity has been
// lost and requires re-bootstrapping.
//
// This method will block and should be run via goroutine.
func (g *Gossip) bootstrap() {
	g.parseBootstrapAddresses()
	for {
		g.mu.Lock()
		if g.closed {
			break
		}
		// Find list of available bootstrap hosts.
		avail := g.filterExtant(g.bootstraps)
		if avail.len() > 0 {
			// Check whether or not we need bootstrap.
			haveClients := g.outgoing.len() > 0
			haveSentinel := g.is.getInfo(SentinelGossip) != nil
			if !haveClients || !haveSentinel {
				// Select a bootstrap address at random and start client.
				addr := avail.selectRandom()
				log.Printf("bootstrapping gossip protocol using host %+v", addr)
				g.startClient(addr)
			}
		}

		// Block until we need bootstrapping again.
		g.stalled.Wait()
		g.mu.Unlock()
	}
}

// manage manages outgoing clients. Periodically, the infostore is
// scanned for infos with hop count exceeding maxToleratedHops()
// threshold. If the number of outgoing clients doesn't exceed
// MaxPeers, a new gossip client is connected to a randomly selected
// peer beyond maxToleratedHops threshold. Otherwise, the least useful
// peer node is cut off to make room for a replacement. Disconnected
// clients are processed via the disconnected channel and taken out of
// the outgoing address set. If there are no longer any outgoing
// connections or the sentinel gossip is unavailable, the bootstrapper
// is notified via the stalled conditional variable.
func (g *Gossip) manage() {
	checkTimeout := time.Tick(g.jitteredGossipInterval())
	// Loop until closed and there are no remaining outgoing connections.
	for {
		select {
		case c := <-g.disconnected:
			g.mu.Lock()
			if c.err != nil {
				log.Printf("client disconnected: %s", c.err)
			}
			g.outgoing.removeAddr(c.addr)
			delete(g.clients, c.addr.String())

			// If the client was disconnected with a forwarding address, connect now.
			if c.forwardAddr != nil {
				g.startClient(c.forwardAddr)
			}
		case <-checkTimeout:
			g.mu.Lock()
			// Check whether the graph needs to be tightened to
			// accommodate distant infos.
			distant := g.filterExtant(g.is.distant(g.maxToleratedHops()))
			if distant.len() > 0 {
				// If we have space, start a client immediately.
				if g.outgoing.len() < MaxPeers {
					g.startClient(distant.selectRandom())
				} else {
					// Otherwise, find least useful peer and close
					// it. Make sure here that we only consider outgoing
					// clients which are connected.
					addr := g.is.leastUseful(g.outgoing.filter(func(a net.Addr) bool {
						return g.clients[a.String()].Client != nil
					}))
					if addr != nil {
						log.Printf("closing least useful client %+v to tighten network graph", addr)
						g.closeClient(addr)
					}
				}
			}
		}

		// If there are no outgoing hosts or sentinel gossip is
		// missing, signal bootstrapper.
		if g.outgoing.len() == 0 && g.filterExtant(g.bootstraps).len() > 0 {
			log.Printf("no outgoing hosts; signaling bootstrap")
			g.stalled.Signal()
		} else if g.is.getInfo(SentinelGossip) == nil {
			log.Printf("missing sentinel gossip %s; assuming partition and reconnecting", SentinelGossip)
			g.stalled.Signal()
		}

		// The exit condition.
		if g.closed && g.outgoing.len() == 0 {
			break
		}
		g.mu.Unlock()
	}

	// Signal exit.
	g.exited <- nil
}

// startClient launches a new client connected to remote address.
// The client is added to the outgoing address set and launched in
// a goroutine.
func (g *Gossip) startClient(addr net.Addr) {
	c := newClient(addr)
	g.outgoing.addAddr(addr)
	g.clients[addr.String()] = c
	go c.start(g, g.disconnected)
}

// closeClient closes an existing client specified by client's
// remote address.
func (g *Gossip) closeClient(addr net.Addr) {
	c := g.clients[addr.String()]
	if c.Client != nil {
		c.Close()
	}
	delete(g.clients, addr.String())
}
