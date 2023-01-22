package utp

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	_ "github.com/anacrolix/envpprof"
	"github.com/anacrolix/missinggo/leaktest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/nettest"
	"golang.org/x/sync/errgroup"
)

var ctx = context.Background()

// newTestUDP returns a UDP PacketConn listening on local host
// it is cleaned up at the end of the test.
func newTestUDP(t testing.TB) net.PacketConn {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { pc.Close() })
	return pc
}

// newTestPC returns a PacketConn for use in the test.
// TODO: make this in process, not UDP
func newTestPC(t testing.TB) net.PacketConn {
	return newTestUDP(t)
}

// newTestSocket sets up a socket using pc as the underlying PacketConn
// the socket is closed at the end of the test
func newTestSocket(t testing.TB, pc net.PacketConn) *Socket {
	opts := []SocketOption{
		WithWriteTimeout(1 * time.Second),
		WithInitialLatency(10 * time.Millisecond),
		WithPacketReadTimeout(2 * time.Second),
	}
	s := NewSocket(pc, opts...)
	t.Cleanup(func() { s.Close() })
	return s
}

func newTestConnPair(t testing.TB) (client, server net.Conn) {
	ssock := newTestSocket(t, newTestPC(t))
	csock := newTestSocket(t, newTestPC(t))
	eg := errgroup.Group{}
	eg.Go(func() (err error) {
		server, err = ssock.Accept()
		return err
	})
	eg.Go(func() (err error) {
		client, err = csock.DialContext(ctx, ssock.LocalAddr())
		return err
	})
	require.NoError(t, eg.Wait())
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	return client, server
}

func TestUTPPingPong(t *testing.T) {
	defer leaktest.GoroutineLeakCheck(t)()
	serverSock := newTestSocket(t, newTestUDP(t))

	pingerClosed := make(chan struct{})
	go func() {
		defer close(pingerClosed)
		clientSock := newTestSocket(t, newTestUDP(t))
		b, err := clientSock.DialContext(ctx, serverSock.LocalAddr())
		require.NoError(t, err)
		defer b.Close()
		n, err := b.Write([]byte("ping"))
		require.NoError(t, err)
		require.EqualValues(t, 4, n)
		buf := make([]byte, 4)
		b.Read(buf)
		require.EqualValues(t, "pong", buf)
	}()
	a, err := serverSock.Accept()
	require.NoError(t, err)
	defer a.Close()
	buf := make([]byte, 42)
	n, err := a.Read(buf)
	require.NoError(t, err)
	require.EqualValues(t, "ping", buf[:n])
	n, err = a.Write([]byte("pong"))
	require.NoError(t, err)
	require.Equal(t, 4, n)
	<-pingerClosed
}

func TestDialTimeout(t *testing.T) {
	defer leaktest.GoroutineLeakCheck(t)()

	s := newTestSocket(t, newTestUDP(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := s.DialContext(ctx, s.Addr())
	require.Equal(t, context.DeadlineExceeded, err)
}

func TestListen(t *testing.T) {
	defer leaktest.GoroutineLeakCheck(t)()

	ln := newTestSocket(t, newTestUDP(t))
	ln.Close()
}

func TestMinMaxHeaderType(t *testing.T) {
	require.Equal(t, stSyn, stMax)
}

func TestConnReadDeadline(t *testing.T) {
	t.Parallel()
	ls := newTestSocket(t, newTestUDP(t))
	ds := newTestSocket(t, newTestUDP(t))
	dcReadErr := make(chan error)
	go func() {
		c, _ := ds.DialContext(ctx, ls.Addr())
		defer c.Close()
		_, err := c.Read(nil)
		dcReadErr <- err
	}()
	c, _ := ls.Accept()
	dl := time.Now().Add(time.Millisecond)
	c.SetReadDeadline(dl)
	_, err := c.Read(nil)
	require.ErrorIs(t, err, ErrTimeout{})
	// The deadline has passed.
	if time.Now().Before(dl) {
		t.Fatal("deadline hasn't passed")
	}
	// Returns timeout on subsequent read.
	_, err = c.Read(nil)
	require.ErrorIs(t, err, ErrTimeout{})
	// Disable the deadline.
	c.SetReadDeadline(time.Time{})
	readReturned := make(chan struct{})
	go func() {
		c.Read(nil)
		close(readReturned)
	}()
	select {
	case <-readReturned:
		// Read returned but shouldn't have.
		t.Fatal("read returned")
	case <-time.After(time.Millisecond):
	}
	c.Close()
	if err := <-dcReadErr; err != io.EOF {
		t.Fatalf("dial conn read returned %s", err)
	}
	select {
	case <-readReturned:
	case <-time.After(time.Millisecond):
		t.Fatal("read should return after Conn is closed")
	}
}

func connectSelfLots(t testing.TB, n int) {
	defer leaktest.GoroutineLeakCheck(t)()

	s := newTestSocket(t, newTestUDP(t))
	eg := errgroup.Group{}
	eg.Go(func() error {
		for i := 0; i < n; i++ {
			c, err := s.Accept()
			if err != nil {
				return err
			}
			defer c.Close()
		}
		return nil
	})
	dialErr := make(chan error)
	connCh := make(chan net.Conn)
	dialSema := make(chan struct{}, DefaultBacklogLen)
	for i := 0; i < n; i++ {
		go func() {
			dialSema <- struct{}{}
			c, err := s.DialContext(ctx, s.Addr())
			<-dialSema
			if err != nil {
				dialErr <- err
				return
			}
			connCh <- c
		}()
	}
	conns := make([]net.Conn, 0, n)
	for i := 0; i < n; i++ {
		select {
		case c := <-connCh:
			conns = append(conns, c)
		case err := <-dialErr:
			t.Fatal(err)
		}
	}
	for _, c := range conns {
		if c != nil {
			c.Close()
		}
	}
	sleepWhile(&s.mu, func() bool { return len(s.conns) != 0 })
	s.Close()
}

// Connect to ourself heaps.
func TestConnectSelf(t *testing.T) {
	// A rough guess says that at worst, I can only have 0x10000/3 connections
	// to the same socket, due to fragmentation in the assigned connection
	// IDs.
	connectSelfLots(t, 0x100)
}

func BenchmarkConnectSelf(b *testing.B) {
	for i := 0; i < b.N; i++ {
		connectSelfLots(b, 2)
	}
}

func BenchmarkNewCloseSocket(b *testing.B) {
	for i := 0; i < b.N; i++ {
		s := NewSocket(newTestUDP(b))
		s.Close()
	}
}

func TestRejectDialBacklogFilled(t *testing.T) {
	s := newTestSocket(t, newTestUDP(t))
	errChan := make(chan error)
	dial := func() {
		_, err := s.DialContext(ctx, s.Addr())
		require.Error(t, err)
		errChan <- err
	}
	// Fill the backlog.
	for i := 0; i < DefaultBacklogLen; i++ {
		go dial()
	}
	sleepWhile(&s.mu, func() bool { return len(s.backlog) < DefaultBacklogLen })
	select {
	case err := <-errChan:
		t.Fatalf("got premature error: %s", err)
	default:
	}
	// One more connection should cause a dial attempt to get reset.
	go dial()
	err := <-errChan
	assert.EqualError(t, err, "peer reset")
	s.Close()
	for i := 0; i < DefaultBacklogLen; i++ {
		<-errChan
	}
}

// Make sure that we can reset AfterFunc timers, so we don't have to create
// brand new ones everytime they fire. Specifically for the Conn resend timer.
func TestResetAfterFuncTimer(t *testing.T) {
	t.Parallel()
	fired := make(chan struct{})
	timer := time.AfterFunc(time.Millisecond, func() {
		fired <- struct{}{}
	})
	<-fired
	if timer.Reset(time.Millisecond) {
		// The timer should have expired
		t.FailNow()
	}
	<-fired
}

func connPairSocket(s *Socket) (initer, accepted net.Conn) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		initer, err = s.DialContext(ctx, s.Addr())
		if err != nil {
			panic(err)
		}
	}()
	accepted, err := s.Accept()
	if err != nil {
		panic(err)
	}
	wg.Wait()
	return
}

// Check that peer sending FIN doesn't cause unread data to be dropped in a
// receiver.
func TestReadFinishedConn(t *testing.T) {
	// TODO: This test injected artifical packet drops.
	a, b := newTestConnPair(t)

	n, err := a.Write([]byte("hello"))
	require.Equal(t, 5, n)
	require.NoError(t, err)
	n, err = a.Write([]byte("world"))
	require.Equal(t, 5, n)
	require.NoError(t, err)

	a.Close()
	all, err := io.ReadAll(b)
	require.NoError(t, err)
	require.EqualValues(t, "helloworld", all)
}

func TestCloseDetachesQuickly(t *testing.T) {
	t.Parallel()
	s := newTestSocket(t, newTestUDP(t))
	go func() {
		clientSock := newTestSocket(t, newTestUDP(t))
		a, err := clientSock.DialContext(ctx, s.LocalAddr())
		if err != nil {
			return
		}
		a.Close()
	}()
	b, _ := s.Accept()
	b.Close()
	sleepWhile(&s.mu, func() bool { return len(s.conns) != 0 })
}

// Check that closing, and resulting detach of a Conn doesn't close the parent
// Socket. We Accept, then close the connection and ensure it's detached. Then
// Accept again to check the Socket is still functional and unclosed.
func TestConnCloseUnclosedSocket(t *testing.T) {
	t.Parallel()
	s := newTestSocket(t, newTestUDP(t))
	// Prevents the dialing goroutine from closing its end of the Conn before
	// we can check that it has been registered in the listener.
	dialerSync := make(chan struct{})

	go func() {
		clientSock := newTestSocket(t, newTestUDP(t))
		for i := 0; i < 2; i++ {
			c, err := clientSock.DialContext(ctx, s.LocalAddr())
			require.NoError(t, err)
			<-dialerSync
			err = c.Close()
			require.NoError(t, err)
		}
	}()
	for i := 0; i < 2; i++ {
		a, err := s.Accept()
		require.NoError(t, err)
		// We do this in a closure because we need to unlock Server.mu if the
		// test failure exception is thrown. "Do as we say, not as we do" -Go
		// team.
		func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			require.Len(t, s.conns, 1)
		}()
		dialerSync <- struct{}{}
		require.NoError(t, a.Close())

		// TODO: remove sleep
		sleepWhile(&s.mu, func() bool { return len(s.conns) != 0 })
	}
}

func TestPacketReadTimeout(t *testing.T) {
	t.Parallel()
	a, _ := newTestConnPair(t)
	_, err := a.Read(nil)
	require.Contains(t, err.Error(), "timeout")
}

func sleepWhile(l sync.Locker, cond func() bool) {
	sleepWhileTimeout(l, cond, -1)
	for {
		l.Lock()
		val := cond()
		l.Unlock()
		if !val {
			break
		}
		time.Sleep(time.Millisecond)
	}
}

func sleepWhileTimeout(l sync.Locker, cond func() bool, timeout time.Duration) {
	var deadline time.Time
	if timeout >= 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		l.Lock()
		val := cond()
		l.Unlock()
		if !val {
			break
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAcceptReturnsAfterClose(t *testing.T) {
	s := newTestSocket(t, newTestPC(t))
	go s.Close()
	_, err := s.Accept()
	t.Log(err)
}

func TestWriteClose(t *testing.T) {
	a, b := newTestConnPair(t)
	defer a.Close()
	defer b.Close()
	a.Write([]byte("hiho"))
	a.Close()
	c, err := io.ReadAll(b)
	require.NoError(t, err)
	require.EqualValues(t, "hiho", c)
	b.Close()
}

// Check that Conn.Write fails when the PacketConn that Socket wraps is
// closed.
func TestWriteUnderlyingPacketConnClosed(t *testing.T) {
	pc := newTestPC(t)
	s := newTestSocket(t, pc)
	dc, ac := connPairSocket(s)
	defer dc.Close()
	defer ac.Close()
	pc.Close()
	n, err := ac.Write([]byte("hello"))
	assert.Equal(t, 0, n)
	// It has to fail. I think it's a race between us writing to the real
	// PacketConn and getting "closed", and the Socket destroying itself, and
	// we get it's destroy error.
	assert.Error(t, err)
	_, err = dc.Read(nil)
	assert.EqualError(t, err, "Socket destroyed")
}

func TestFillBuffers(t *testing.T) {
	a, b := newTestConnPair(t)
	defer b.Close()
	var sent []byte
	for {
		buf := make([]byte, 100000)
		io.ReadFull(rand.Reader, buf)
		a.SetWriteDeadline(time.Now().Add(5 * time.Second))
		n, err := a.Write(buf)
		sent = append(sent, buf[:n]...)
		if err != nil {
			// Receiver will stop processing packets, packets will be dropped,
			// and thus not acked.
			assert.ErrorIs(t, err, ErrTimeout{IsAck: true})
			break
		}
		require.NotEqual(t, 0, n)
	}
	t.Logf("buffered %d bytes", len(sent))
	a.Close()
	all, err := io.ReadAll(b)
	assert.NoError(t, err)
	assert.EqualValues(t, len(sent), len(all))
	assert.EqualValues(t, sent, all)
}

func BenchmarkEchoLongBuffer(tb *testing.B) {
	pristine := make([]byte, 3000000)
	n, err := io.ReadFull(rand.Reader, pristine)
	require.EqualValues(tb, len(pristine), n)
	require.NoError(tb, err)
	tb.SetBytes(int64(len(pristine)))
	tb.ResetTimer()
	for i := 0; i < tb.N; i++ {
		func() {
			a, b := newTestConnPair(tb)
			defer a.Close()
			defer b.Close()
			go func() {
				n, err := io.Copy(b, b)
				require.NoError(tb, err)
				require.EqualValues(tb, len(pristine), n)
				b.Close()
			}()
			go func() {
				n, err := a.Write(pristine)
				require.NoError(tb, err)
				require.EqualValues(tb, len(pristine), n)
			}()
			echo := make([]byte, len(pristine))
			n, err := io.ReadFull(a, echo)
			a.Close()
			assert.NoError(tb, err)
			require.EqualValues(tb, len(echo), n)
			require.True(tb, bytes.Equal(pristine, echo))
		}()
	}
}

// Create a utp.Conn between two Sockets. Then axe one before it can cry for
// help. Check that the other still destroys its underlying UDP PacketConn
// when the Socket and Conn are closed.
func TestSocketDestroyedConnsClosedTimeout(t *testing.T) {
	s1pc := newTestUDP(t)
	s1 := newTestSocket(t, s1pc)
	s2pc := newTestUDP(t)
	s2 := newTestSocket(t, s2pc)
	accepted := make(chan struct{})
	var s1c net.Conn
	go func() {
		var err error
		s1c, err = s1.Accept()
		close(accepted)
		require.NoError(t, err)
	}()
	s2c, err := s2.DialContext(ctx, s1.Addr())
	require.NoError(t, err)
	<-accepted
	// Axe Socket 1's PacketConn.
	s1pc.Close()
	// Now its wrappers, that may be confused right now.
	s1.Close()
	// s1 should have been destroyed.
	<-s1.destroyed.LockedChan(&s1.mu)
	s1c.Close()
	// Check that we can now listen in Socket 1's place.
	for {
		pc, err := net.ListenPacket("udp", s1pc.LocalAddr().String())
		if err == nil {
			pc.Close()
			break
		}
		time.Sleep(time.Millisecond)
	}
	// Maybe bump up the unacked sends a bit.
	s2c.Write([]byte("ping"))
	s2c.Write([]byte("pong"))
	s2c.Close()
	s2.Close()
	// s2 should be destroyed when the write timeout on s2c occurs.
	<-s2.destroyed.LockedChan(&s2.mu)
}

// Make sure that creating two connecting sockets repeatedly will successfully
// close the sockets after calling CloseNow(). New sockets can be created with
// the same address without a "bind: address already in use" error.
func TestCloseNow(t *testing.T) {
	s1Addr := "localhost:0"
	s2Addr := "localhost:0"
	for i := 0; i < 100; i++ {
		pc1, err := net.ListenPacket("udp", s1Addr)
		require.NoError(t, err)
		pc2, err := net.ListenPacket("udp", s2Addr)
		require.NoError(t, err)

		s1 := newTestSocket(t, pc1)
		s2 := newTestSocket(t, pc2)

		go func() {
			c, err := s1.DialContext(ctx, s2.Addr())
			assert.NoError(t, err)
			_, err = c.Write([]byte("ping"))
			assert.NoError(t, err)
			_, err = c.Read(nil)
			assert.Equal(t, err.Error(), "EOF")
		}()
		c, _ := s2.Accept()
		buf := make([]byte, 4)
		_, err = c.Read(buf)
		assert.NoError(t, err)
		s1.CloseNow()
		s2.CloseNow()
		_, err = c.Read(nil)
		assert.Equal(t, err.Error(), "EOF")
	}
}

// Test dial 0.0.0.0
func TestZerosIPV4(t *testing.T) {
	t.Skip("TODO")
	//if runtime.GOOS == "windows" {
	//	t.Skip("dialling 0.0.0.0 not working on windows before go1.8?")
	//}
	testSimpleRead(t, "0.0.0.0", "0.0.0.0")
}

// Test dial [::]
func TestZerosIPV6(t *testing.T) {
	t.Skip("TODO")
	//if runtime.GOOS == "windows" {
	//	t.Skip("dialling 0.0.0.0 not working on windows before go1.8?")
	//}
	testSimpleRead(t, "[::]", "[::]")
}

// tests a simple server accept/write/close with client dial/read.
func testSimpleRead(t *testing.T, serverBindIP string, clientDialIP string) {
	pc1, err := net.ListenPacket("udp", fmt.Sprintf("%s:0", serverBindIP))
	require.NoError(t, err)
	serverSock := newTestSocket(t, pc1)

	sport := pc1.(*net.UDPConn).LocalAddr().(*net.UDPAddr).Port

	serr := make(chan error, 1)
	go func() {
		con, err := serverSock.Accept()
		if err != nil {
			serr <- err
			return
		}
		io.WriteString(con, "hello")
		serr <- con.Close()
	}()

	pc2, err := net.ListenPacket("udp", fmt.Sprintf("%s:%d", clientDialIP, sport))
	require.NoError(t, err)
	clientSock := newTestSocket(t, pc2)
	client, err := clientSock.DialContext(ctx, serverSock.LocalAddr())
	require.NoError(t, err)
	defer client.Close()

	response, err := io.ReadAll(client)
	require.NoError(t, err)

	require.Equal(t, "hello", string(response))
}

func TestNettestInprocSocket(t *testing.T) {
	nettest.TestConn(t, func() (c1, c2 net.Conn, stop func(), err error) {
		s := newTestSocket(t, newTestPC(t))
		c1, c2 = connPairSocket(s)
		stop = func() {
			s.CloseNow()
		}
		return
	})
}

func TestNettestLocalhostUDP(t *testing.T) {
	t.Skip("flaky")
	nettest.TestConn(t, func() (c1, c2 net.Conn, stop func(), err error) {
		s := NewSocket(newTestUDP(t))
		c1, c2 = connPairSocket(s)
		stop = func() {
			s.CloseNow()
		}
		return
	})
}
