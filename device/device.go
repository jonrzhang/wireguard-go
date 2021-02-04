/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2021 WireGuard LLC. All Rights Reserved.
 */

package device

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tailscale/wireguard-go/conn"
	"github.com/tailscale/wireguard-go/ratelimiter"
	"github.com/tailscale/wireguard-go/rwcancel"
	"github.com/tailscale/wireguard-go/tun"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

type Device struct {
	isUp           AtomicBool // device is (going) up
	isClosed       AtomicBool // device is closed? (acting as guard)
	log            *Logger
	handshakeDone  func(peerKey NoisePublicKey, peer *Peer, allowedIPs *AllowedIPs)
	skipBindUpdate bool
	createBind     func(uport uint16, device *Device) (conn.Bind, uint16, error)
	createEndpoint func(key [32]byte, s string) (conn.Endpoint, error)

	// synchronized resources (locks acquired in order)

	state struct {
		stopping sync.WaitGroup
		sync.Mutex
		changing AtomicBool
		current  bool
	}

	net struct {
		stopping sync.WaitGroup
		sync.RWMutex
		bind          conn.Bind // bind interface
		netlinkCancel *rwcancel.RWCancel
		port          uint16 // listening port
		fwmark        uint32 // mark value (0 = disabled)
	}

	staticIdentity struct {
		sync.RWMutex
		privateKey NoisePrivateKey
		publicKey  NoisePublicKey
	}

	peers struct {
		empty        AtomicBool // empty reports whether len(keyMap) == 0
		sync.RWMutex            // protects keyMap
		keyMap       map[NoisePublicKey]*Peer
	}

	// unprotected / "self-synchronising resources"

	allowedips    AllowedIPs
	indexTable    IndexTable
	cookieChecker CookieChecker

	rate struct {
		underLoadUntil int64
		limiter        ratelimiter.Ratelimiter
	}

	pool struct {
		messageBuffers   *WaitPool
		inboundElements  *WaitPool
		outboundElements *WaitPool
	}

	queue struct {
		encryption *outboundQueue
		decryption *inboundQueue
		handshake  *handshakeQueue
	}

	tun struct {
		device tun.Device
		mtu    int32
	}

	ipcMutex sync.RWMutex
	closed   chan struct{}
}

// An outboundQueue is a channel of QueueOutboundElements awaiting encryption.
// An outboundQueue is ref-counted using its wg field.
// An outboundQueue created with newOutboundQueue has one reference.
// Every additional writer must call wg.Add(1).
// Every completed writer must call wg.Done().
// When no further writers will be added,
// call wg.Done to remove the initial reference.
// When the refcount hits 0, the queue's channel is closed.
type outboundQueue struct {
	c  chan *QueueOutboundElement
	wg sync.WaitGroup
}

func newOutboundQueue() *outboundQueue {
	q := &outboundQueue{
		c: make(chan *QueueOutboundElement, QueueOutboundSize),
	}
	q.wg.Add(1)
	go func() {
		q.wg.Wait()
		close(q.c)
	}()
	return q
}

// A inboundQueue is similar to an outboundQueue; see those docs.
type inboundQueue struct {
	c  chan *QueueInboundElement
	wg sync.WaitGroup
}

func newInboundQueue() *inboundQueue {
	q := &inboundQueue{
		c: make(chan *QueueInboundElement, QueueInboundSize),
	}
	q.wg.Add(1)
	go func() {
		q.wg.Wait()
		close(q.c)
	}()
	return q
}

// A handshakeQueue is similar to an outboundQueue; see those docs.
type handshakeQueue struct {
	c  chan QueueHandshakeElement
	wg sync.WaitGroup
}

func newHandshakeQueue() *handshakeQueue {
	q := &handshakeQueue{
		c: make(chan QueueHandshakeElement, QueueHandshakeSize),
	}
	q.wg.Add(1)
	go func() {
		q.wg.Wait()
		close(q.c)
	}()
	return q
}

/* Converts the peer into a "zombie", which remains in the peer map,
 * but processes no packets and does not exists in the routing table.
 *
 * Must hold device.peers.Mutex
 */
func unsafeRemovePeer(device *Device, peer *Peer, key NoisePublicKey) {

	// stop routing and processing of packets

	device.allowedips.RemoveByPeer(peer)
	peer.Stop()

	// remove from peer map

	delete(device.peers.keyMap, key)
	device.peers.empty.Set(len(device.peers.keyMap) == 0)
}

func deviceUpdateState(device *Device) {

	// check if state already being updated (guard)

	if device.state.changing.Swap(true) {
		return
	}

	// compare to current state of device

	device.state.Lock()

	newIsUp := device.isUp.Get()

	if newIsUp == device.state.current {
		device.state.changing.Set(false)
		device.state.Unlock()
		return
	}

	// change state of device

	switch newIsUp {
	case true:
		if err := device.BindUpdate(); err != nil {
			device.log.Errorf("Unable to update bind: %v", err)
			device.isUp.Set(false)
			break
		}
		device.peers.RLock()
		for _, peer := range device.peers.keyMap {
			peer.Start()
			if atomic.LoadUint32(&peer.persistentKeepaliveInterval) > 0 {
				peer.SendKeepalive()
			}
		}
		device.peers.RUnlock()

	case false:
		device.BindClose()
		device.peers.RLock()
		for _, peer := range device.peers.keyMap {
			peer.Stop()
		}
		device.peers.RUnlock()
	}

	// update state variables

	device.state.current = newIsUp
	device.state.changing.Set(false)
	device.state.Unlock()

	// check for state change in the mean time

	deviceUpdateState(device)
}

func (device *Device) Up() {

	// closed device cannot be brought up

	if device.isClosed.Get() {
		return
	}

	device.isUp.Set(true)
	deviceUpdateState(device)
}

func (device *Device) Down() {
	device.isUp.Set(false)
	deviceUpdateState(device)
}

func (device *Device) IsUnderLoad() bool {
	// check if currently under load
	now := time.Now()
	underLoad := len(device.queue.handshake.c) >= UnderLoadQueueSize
	if underLoad {
		atomic.StoreInt64(&device.rate.underLoadUntil, now.Add(UnderLoadAfterTime).UnixNano())
		return true
	}
	// check if recently under load
	return atomic.LoadInt64(&device.rate.underLoadUntil) > now.UnixNano()
}

func (device *Device) SetPrivateKey(sk NoisePrivateKey) error {
	// lock required resources

	device.staticIdentity.Lock()
	defer device.staticIdentity.Unlock()

	if sk.Equals(device.staticIdentity.privateKey) {
		return nil
	}

	device.peers.Lock()
	defer device.peers.Unlock()

	lockedPeers := make([]*Peer, 0, len(device.peers.keyMap))
	for _, peer := range device.peers.keyMap {
		peer.handshake.mutex.RLock()
		lockedPeers = append(lockedPeers, peer)
	}

	// remove peers with matching public keys
	var publicKey NoisePublicKey // allow a zero key here for disabling this device
	if !sk.IsZero() {
		publicKey = sk.publicKey()
	}

	for key, peer := range device.peers.keyMap {
		if peer.handshake.remoteStatic.Equals(publicKey) {
			peer.handshake.mutex.RUnlock()
			unsafeRemovePeer(device, peer, key)
			peer.handshake.mutex.RLock()
		}
	}

	// update key material

	device.staticIdentity.privateKey = sk
	device.staticIdentity.publicKey = publicKey
	device.cookieChecker.Init(publicKey)

	// do static-static DH pre-computations

	expiredPeers := make([]*Peer, 0, len(device.peers.keyMap))
	for _, peer := range device.peers.keyMap {
		handshake := &peer.handshake
		handshake.precomputedStaticStatic = device.staticIdentity.privateKey.sharedSecret(handshake.remoteStatic)
		expiredPeers = append(expiredPeers, peer)
	}

	for _, peer := range lockedPeers {
		peer.handshake.mutex.RUnlock()
	}
	for _, peer := range expiredPeers {
		peer.ExpireCurrentKeypairs()
	}

	return nil
}

type DeviceOptions struct {
	Logger *Logger

	// HandshakeDone is called every time we complete a peer handshake.
	HandshakeDone func(peerKey NoisePublicKey, peer *Peer, allowedIPs *AllowedIPs)

	CreateEndpoint func(key [32]byte, s string) (conn.Endpoint, error)
	CreateBind     func(uport uint16) (conn.Bind, uint16, error)
	SkipBindUpdate bool // if true, CreateBind only ever called once
}

func NewDevice(tunDevice tun.Device, opts *DeviceOptions) *Device {
	device := new(Device)
	device.closed = make(chan struct{})

	device.isUp.Set(false)
	device.isClosed.Set(false)

	if opts != nil {
		if opts.Logger != nil {
			device.log = opts.Logger
		}
		device.handshakeDone = opts.HandshakeDone
		if opts.CreateEndpoint != nil {
			device.createEndpoint = opts.CreateEndpoint
		} else {
			device.createEndpoint = func(_ [32]byte, s string) (conn.Endpoint, error) {
				return conn.CreateEndpoint(s)
			}
		}
		if opts.CreateBind != nil {
			device.createBind = func(uport uint16, device *Device) (conn.Bind, uint16, error) {
				return opts.CreateBind(uport)
			}
		} else {
			device.createBind = func(uport uint16, device *Device) (conn.Bind, uint16, error) {
				return conn.CreateBind(uport)
			}
		}
		device.skipBindUpdate = opts.SkipBindUpdate
	}

	device.tun.device = tunDevice
	mtu, err := device.tun.device.MTU()
	if err != nil {
		device.log.Errorf("Trouble determining MTU, assuming default: %v", err)
		mtu = DefaultMTU
	}
	device.tun.mtu = int32(mtu)
	device.peers.keyMap = make(map[NoisePublicKey]*Peer)
	device.rate.limiter.Init()
	device.indexTable.Init()
	device.PopulatePools()

	// create queues

	device.queue.handshake = newHandshakeQueue()
	device.queue.encryption = newOutboundQueue()
	device.queue.decryption = newInboundQueue()

	// prepare net

	device.net.port = 0
	device.net.bind = nil

	// start workers

	cpus := runtime.NumCPU()
	device.state.stopping.Wait()
	for i := 0; i < cpus; i++ {
		device.state.stopping.Add(2) // decryption and handshake
		go device.RoutineEncryption()
		go device.RoutineDecryption()
		go device.RoutineHandshake()
	}

	device.state.stopping.Add(2)
	go device.RoutineReadFromTUN()
	go device.RoutineTUNEventReader()

	return device
}

func (device *Device) LookupPeer(pk NoisePublicKey) *Peer {
	device.peers.RLock()
	defer device.peers.RUnlock()

	return device.peers.keyMap[pk]
}

func (device *Device) RemovePeer(key NoisePublicKey) {
	device.peers.Lock()
	defer device.peers.Unlock()
	// stop peer and remove from routing

	peer, ok := device.peers.keyMap[key]
	if ok {
		unsafeRemovePeer(device, peer, key)
	}
}

func (device *Device) RemoveAllPeers() {
	device.peers.Lock()
	defer device.peers.Unlock()

	for key, peer := range device.peers.keyMap {
		unsafeRemovePeer(device, peer, key)
	}

	device.peers.keyMap = make(map[NoisePublicKey]*Peer)
}

func (device *Device) Close() {
	if device.isClosed.Swap(true) {
		return
	}

	device.log.Verbosef("Device closing")
	device.state.changing.Set(true)
	device.state.Lock()
	defer device.state.Unlock()

	device.tun.device.Close()
	device.BindClose()

	device.isUp.Set(false)

	// Remove peers before closing queues,
	// because peers assume that queues are active.
	device.RemoveAllPeers()

	// We kept a reference to the encryption and decryption queues,
	// in case we started any new peers that might write to them.
	// No new peers are coming; we are done with these queues.
	device.queue.encryption.wg.Done()
	device.queue.decryption.wg.Done()
	device.queue.handshake.wg.Done()
	device.state.stopping.Wait()

	device.rate.limiter.Close()

	device.state.changing.Set(false)
	device.log.Verbosef("Interface closed")
	close(device.closed)
}

func (device *Device) Wait() chan struct{} {
	return device.closed
}

func (device *Device) SendKeepalivesToPeersWithCurrentKeypair() {
	if device.isClosed.Get() {
		return
	}

	device.peers.RLock()
	for _, peer := range device.peers.keyMap {
		peer.keypairs.RLock()
		sendKeepalive := peer.keypairs.current != nil && !peer.keypairs.current.created.Add(RejectAfterTime).Before(time.Now())
		peer.keypairs.RUnlock()
		if sendKeepalive {
			peer.SendKeepalive()
		}
	}
	device.peers.RUnlock()
}

func unsafeCloseBind(device *Device) error {
	var err error
	netc := &device.net
	if netc.netlinkCancel != nil {
		netc.netlinkCancel.Cancel()
	}
	if netc.bind != nil {
		err = netc.bind.Close()
		netc.bind = nil
	}
	netc.stopping.Wait()
	return err
}

func (device *Device) Bind() conn.Bind {
	device.net.Lock()
	defer device.net.Unlock()
	return device.net.bind
}

func (device *Device) BindSetMark(mark uint32) error {

	device.net.Lock()
	defer device.net.Unlock()

	// check if modified

	if device.net.fwmark == mark {
		return nil
	}

	// update fwmark on existing bind

	device.net.fwmark = mark
	if device.isUp.Get() && device.net.bind != nil {
		if err := device.net.bind.SetMark(mark); err != nil {
			return err
		}
	}

	// clear cached source addresses

	device.peers.RLock()
	for _, peer := range device.peers.keyMap {
		peer.Lock()
		defer peer.Unlock()
		if peer.endpoint != nil {
			peer.endpoint.ClearSrc()
		}
	}
	device.peers.RUnlock()

	return nil
}

func (device *Device) BindUpdate() error {

	device.net.Lock()
	defer device.net.Unlock()

	if device.skipBindUpdate && device.net.bind != nil {
		device.log.Verbosef("UDP bind update skipped")
		return nil
	}

	// close existing sockets

	if err := unsafeCloseBind(device); err != nil {
		return err
	}

	// open new sockets

	if device.isUp.Get() {

		// bind to new port

		var err error
		netc := &device.net
		netc.bind, netc.port, err = device.createBind(netc.port, device)
		if err != nil {
			netc.bind = nil
			netc.port = 0
			return err
		}
		netc.netlinkCancel, err = device.startRouteListener(netc.bind)
		if err != nil {
			netc.bind.Close()
			netc.bind = nil
			netc.port = 0
			return err
		}

		// set fwmark

		if netc.fwmark != 0 {
			err = netc.bind.SetMark(netc.fwmark)
			if err != nil {
				return err
			}
		}

		// clear cached source addresses

		device.peers.RLock()
		for _, peer := range device.peers.keyMap {
			peer.Lock()
			defer peer.Unlock()
			if peer.endpoint != nil {
				peer.endpoint.ClearSrc()
			}
		}
		device.peers.RUnlock()

		// start receiving routines

		device.net.stopping.Add(2)
		device.queue.decryption.wg.Add(2) // each RoutineReceiveIncoming goroutine writes to device.queue.decryption
		device.queue.handshake.wg.Add(2)  // each RoutineReceiveIncoming goroutine writes to device.queue.handshake
		go device.RoutineReceiveIncoming(ipv4.Version, netc.bind)
		go device.RoutineReceiveIncoming(ipv6.Version, netc.bind)

		device.log.Verbosef("UDP bind has been updated")
	}

	return nil
}

func (device *Device) BindClose() error {
	device.net.Lock()
	err := unsafeCloseBind(device)
	device.net.Unlock()
	return err
}
