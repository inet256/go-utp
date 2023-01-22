package utp

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/anacrolix/missinggo"
	"github.com/anacrolix/missinggo/pproffd"
	"github.com/brendoncarroll/stdctx/logctx"
	"github.com/brendoncarroll/stdctx/units"
)

var _ net.Listener = &Socket{}

// Uniquely identifies any uTP connection on top of the underlying packet
// stream.
type connKey struct {
	remoteAddr string
	connID     uint16
}

func newConnKey(raddr net.Addr, connID uint16) connKey {
	return connKey{raddr.String(), connID}
}

// A Socket wraps a net.PacketConn, diverting uTP packets to its child uTP
// Conns.
type Socket struct {
	config socketConfig

	pc net.PacketConn

	mu    sync.RWMutex
	conns map[connKey]*Conn

	backlogNotEmpty missinggo.Event
	backlog         map[syn]struct{}

	closed    missinggo.Event
	destroyed missinggo.Event

	wgReadWrite sync.WaitGroup

	unusedReads chan read
	connDeadlines
	// If a read error occurs on the underlying net.PacketConn, it is put
	// here. This is because reading is done in its own goroutine to dispatch
	// to uTP Conns.
	ReadErr error
}

// NewSocket creates a net.PacketConn with the given network and address, and
// returns a Socket dispatching on it.
// Create a Socket, using the provided net.PacketConn. If you want to retain
// use of the net.PacketConn after the Socket closes it, override the
// net.PacketConn's Close method, or use NetSocketFromPacketConnNoClose.
func NewSocket(pc net.PacketConn, opts ...SocketOption) *Socket {
	config := defaultSocketConfig()
	for _, opt := range opts {
		opt(&config)
	}
	s := &Socket{
		config:      config,
		backlog:     make(map[syn]struct{}, config.backlogLen),
		pc:          pc,
		unusedReads: make(chan read, 100),
		wgReadWrite: sync.WaitGroup{},
	}
	s.connDeadlines = newConnDeadlines(&s.mu)
	go s.reader()
	return s
}

func (s *Socket) DialContext(ctx context.Context, addr net.Addr) (nc net.Conn, err error) {
	c, err := s.startOutboundConn(addr)
	if err != nil {
		return
	}

	connErr := make(chan error, 1)
	go func() {
		connErr <- c.recvSynAck()
	}()
	select {
	case err = <-connErr:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		s.mu.Lock()
		c.destroy(errors.New("dial timeout"))
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	c.updateCanWrite()
	s.mu.Unlock()
	nc = pproffd.WrapNetConn(c)
	return
}

// LocalAddr returns the local address of the underlying socket.
func (s *Socket) LocalAddr() net.Addr {
	return s.pc.LocalAddr()
}

// ReadFrom
// DEPRECATED:
func (s *Socket) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	select {
	case read, ok := <-s.unusedReads:
		if !ok {
			err = io.EOF
			return
		}
		n = copy(p, read.data)
		addr = read.from
		return
	case <-s.connDeadlines.read.passed.LockedChan(&s.mu):
		err = ErrTimeout{}
		return
	}
}

// WriteTo
// DEPRECATED
func (s *Socket) WriteTo(b []byte, addr net.Addr) (n int, err error) {
	s.mu.Lock()
	if s.connDeadlines.write.passed.IsSet() {
		err = ErrTimeout{}
	}
	s.wgReadWrite.Add(1)
	defer s.wgReadWrite.Done()
	s.mu.Unlock()
	if err != nil {
		return
	}
	return s.pc.WriteTo(b, addr)
}

func (s *Socket) unusedRead(read read) {
	telemIncr(context.TODO(), "unusedReads", int(1), units.None)
	select {
	case s.unusedReads <- read:
	default:
		// Drop the packet.
		telemIncr(context.TODO(), "unusedReadsDropped", int(1), units.None)
	}
}

func (s *Socket) pushBacklog(syn syn) {
	if _, ok := s.backlog[syn]; ok {
		return
	}
	// Pop a pseudo-random syn to make room. TODO: Use missinggo/orderedmap,
	// coz that's what is wanted here.
	for k := range s.backlog {
		if len(s.backlog) < s.config.backlogLen {
			break
		}
		delete(s.backlog, k)
		// A syn is sent on the remote's recv_id, so this is where we can send
		// the reset.
		s.reset(k.addr, k.seq_nr, k.conn_id)
	}
	s.backlog[syn] = struct{}{}
	s.backlogChanged()
}

func (s *Socket) reader() {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.destroy()
	var b [maxRecvSize]byte
	for {
		s.wgReadWrite.Add(1)
		s.mu.Unlock()
		n, addr, err := s.pc.ReadFrom(b[:])
		s.wgReadWrite.Done()
		s.mu.Lock()
		if s.destroyed.IsSet() {
			return
		}
		if err != nil {
			logctx.Errorf(context.TODO(), "error reading Socket PacketConn: %s", err)
			s.ReadErr = err
			return
		}
		s.handleReceivedPacket(read{
			append([]byte(nil), b[:n]...),
			addr,
		})
	}
}

func receivedUTPPacketSize(n int) {
	telemMark(context.TODO(), "largestReceivedUTPPacket", int64(n), units.Bytes)
}

func (s *Socket) connForRead(h header, from net.Addr) (c *Conn, ok bool) {
	c, ok = s.conns[connKey{
		remoteAddr: from.String(),
		connID: func() uint16 {
			if h.Type == stSyn {
				// SYNs have a ConnID one lower than the eventual recvID, and we index
				// the connections with that, so use it for the lookup.
				return h.ConnID + 1
			} else {
				return h.ConnID
			}
		}(),
	}]
	return
}

func (s *Socket) handlePacketReceivedForEstablishedConn(h header, from net.Addr, data []byte, c *Conn) {
	if h.Type == stSyn {
		if h.ConnID == c.send_id-2 {
			// This is a SYN for connection that cannot exist locally. The
			// connection the remote wants to establish here with the proposed
			// recv_id, already has an existing connection that was dialled
			// *out* from this socket, which is why the send_id is 1 higher,
			// rather than 1 lower than the recv_id.
			logctx.Warnf(context.TODO(), "resetting conflicting syn")
			s.reset(from, h.SeqNr, h.ConnID)
			return
		} else if h.ConnID != c.send_id {
			panic("bad assumption")
		}
	}
	c.receivePacket(h, data)
}

func (s *Socket) handleReceivedPacket(p read) {
	if len(p.data) < 20 {
		s.unusedRead(p)
		return
	}
	var h header
	hEnd, err := h.Unmarshal(p.data)
	if err != nil || h.Type > stMax || h.Version != 1 {
		s.unusedRead(p)
		return
	}
	if c, ok := s.connForRead(h, p.from); ok {
		receivedUTPPacketSize(len(p.data))
		s.handlePacketReceivedForEstablishedConn(h, p.from, p.data[hEnd:], c)
		return
	}
	// Packet doesn't belong to an existing connection.
	switch h.Type {
	case stSyn:
		s.pushBacklog(syn{
			seq_nr:  h.SeqNr,
			conn_id: h.ConnID,
			addr:    p.from,
		})
		return
	case stReset:
		// Could be a late arriving packet for a Conn we're already done with.
		// If it was for an existing connection, we would have handled it
		// earlier.
	default:
		telemIncr(context.TODO(), "unexpectedPacketsRead", int(1), units.Bytes)
		// This is an unexpected packet. We'll send a reset, but also pass it
		// on. I don't think you can reset on the received packets ConnID if
		// it isn't a SYN, as the send_id will differ in this case.
		s.reset(p.from, h.SeqNr, h.ConnID)
		// Connection initiated by remote.
		s.reset(p.from, h.SeqNr, h.ConnID-1)
		// Connection initiated locally.
		s.reset(p.from, h.SeqNr, h.ConnID+1)
	}
	s.unusedRead(p)
}

// Send a reset in response to a packet with the given header.
func (s *Socket) reset(addr net.Addr, ackNr, connId uint16) {
	b := make([]byte, 0, maxHeaderSize)
	h := header{
		Type:    stReset,
		Version: 1,
		ConnID:  connId,
		AckNr:   ackNr,
	}
	b = b[:h.Marshal(b)]
	go s.writeTo(b, addr)
}

// Return a recv_id that should be free. Handling the case where it isn't is
// deferred to a more appropriate function.
func (s *Socket) newConnID(remoteAddr net.Addr) (id uint16) {
	// Rather than use math.Rand, which requires generating all the IDs up
	// front and allocating a slice, we do it on the stack, generating the IDs
	// only as required. To do this, we use the fact that the array is
	// default-initialized. IDs that are 0, are actually their index in the
	// array. IDs that are non-zero, are +1 from their intended ID.
	var idsBack [0x10000]int
	ids := idsBack[:]
	for len(ids) != 0 {
		// Pick the next ID from the untried ids.
		i := rand.Intn(len(ids))
		id = uint16(ids[i])
		// If it's zero, then treat it as though the index i was the ID.
		// Otherwise the value we get is the ID+1.
		if id == 0 {
			id = uint16(i)
		} else {
			id--
		}
		// Check there's no connection using this ID for its recv_id...
		_, ok1 := s.conns[newConnKey(remoteAddr, id)]
		// and if we're connecting to our own Socket, that there isn't a Conn
		// already receiving on what will correspond to our send_id. Note that
		// we just assume that we could be connecting to our own Socket. This
		// will halve the available connection IDs to each distinct remote
		// address. Presumably that's ~0x8000, down from ~0x10000.
		_, ok2 := s.conns[newConnKey(remoteAddr, id+1)]
		_, ok4 := s.conns[newConnKey(remoteAddr, id-1)]
		if !ok1 && !ok2 && !ok4 {
			return
		}
		// The set of possible IDs is shrinking. The highest one will be lost, so
		// it's moved to the location of the one we just tried.
		ids[i] = len(ids) // Conveniently already +1.
		// And shrink.
		ids = ids[:len(ids)-1]
	}
	return
}

func (s *Socket) newConn(addr net.Addr) (c *Conn) {
	c = &Conn{
		socket:           s,
		remoteSocketAddr: addr,
		created:          time.Now(),
	}
	c.sendPendingSendSendStateTimer = missinggo.StoppedFuncTimer(c.sendPendingSendStateTimerCallback)
	c.packetReadTimeoutTimer = time.AfterFunc(s.config.packetReadTimeout, c.receivePacketTimeoutCallback)
	return
}

func (s *Socket) startOutboundConn(addr net.Addr) (c *Conn, err error) {
	ctx := context.TODO()
	s.mu.Lock()
	defer s.mu.Unlock()
	c = s.newConn(addr)
	c.recv_id = s.newConnID(c.RemoteAddr())
	c.send_id = c.recv_id + 1
	logctx.Debugf(ctx, "dial registering addr: %s", c.RemoteAddr().String())
	if !s.registerConn(c.recv_id, c.RemoteAddr(), c) {
		err = errors.New("couldn't register new connection")
		logctx.Debugln(ctx, c.recv_id, c.RemoteAddr().String())
		for k, c := range s.conns {
			logctx.Debugln(ctx, k, c, c.age())
		}
		logctx.Debugf(ctx, "that's %d connections", len(s.conns))
	}
	if err != nil {
		return
	}
	c.seq_nr = 1
	c.writeSyn()
	return
}

func (me *Socket) writeTo(b []byte, addr net.Addr) (n int, err error) {
	return me.pc.WriteTo(b, addr)
}

// Returns true if the connection was newly registered, false otherwise.
func (s *Socket) registerConn(recvID uint16, remoteAddr net.Addr, c *Conn) bool {
	if s.conns == nil {
		s.conns = make(map[connKey]*Conn)
	}
	key := connKey{remoteAddr.String(), recvID}
	if _, ok := s.conns[key]; ok {
		return false
	}
	c.connKey = key
	s.conns[key] = c
	return true
}

func (s *Socket) backlogChanged() {
	if len(s.backlog) != 0 {
		s.backlogNotEmpty.Set()
	} else {
		s.backlogNotEmpty.Clear()
	}
}

func (s *Socket) nextSyn() (syn syn, err error) {
	for {
		missinggo.WaitEvents(&s.mu, &s.closed, &s.backlogNotEmpty, &s.destroyed)
		if s.closed.IsSet() {
			err = ErrClosed
			return
		}
		if s.destroyed.IsSet() {
			err = s.ReadErr
			return
		}
		for k := range s.backlog {
			syn = k
			delete(s.backlog, k)
			s.backlogChanged()
			return
		}
	}
}

// ACK a SYN, and return a new Conn for it. ok is false if the SYN is bad, and
// the Conn invalid.
func (s *Socket) ackSyn(syn syn) (c *Conn, ok bool) {
	c = s.newConn(syn.addr)
	c.send_id = syn.conn_id
	c.recv_id = c.send_id + 1
	c.seq_nr = uint16(rand.Int())
	c.lastAck = c.seq_nr - 1
	c.ack_nr = syn.seq_nr
	c.synAcked = true
	c.updateCanWrite()
	if !s.registerConn(c.recv_id, syn.addr, c) {
		// SYN that triggered this accept duplicates existing connection.
		// Ack again in case the SYN was a resend.
		c = s.conns[newConnKey(syn.addr, c.recv_id)]
		if c.send_id != syn.conn_id {
			panic(":|")
		}
		c.sendState()
		return
	}
	c.sendState()
	ok = true
	return
}

// Accept and return a new uTP connection.
func (s *Socket) Accept() (net.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		syn, err := s.nextSyn()
		if err != nil {
			return nil, err
		}
		c, ok := s.ackSyn(syn)
		if ok {
			c.updateCanWrite()
			return c, nil
		}
	}
}

// The address we're listening on for new uTP connections.
func (s *Socket) Addr() net.Addr {
	return s.pc.LocalAddr()
}

func (s *Socket) CloseNow() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed.Set()
	for _, c := range s.conns {
		c.closeNow()
	}
	s.destroy()
	s.wgReadWrite.Wait()
	return nil
}

func (s *Socket) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed.Set()
	s.lazyDestroy()
	return nil
}

func (s *Socket) lazyDestroy() {
	if len(s.conns) != 0 {
		return
	}
	if !s.closed.IsSet() {
		return
	}
	s.destroy()
}

func (s *Socket) destroy() {
	s.destroyed.Set()
	s.pc.Close()
	for _, c := range s.conns {
		c.destroy(errors.New("Socket destroyed"))
	}
}
