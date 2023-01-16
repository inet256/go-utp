package utp

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"github.com/anacrolix/missinggo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcceptOnDestroyedSocket(t *testing.T) {
	pc := newTestUDP(t)
	s := NewSocket(pc)
	go pc.Close()
	_, err := s.Accept()
	require.Error(t, err)
	t.Log(err.Error())
}

func TestSocketDeadlines(t *testing.T) {
	pc := newTestUDP(t)
	s := NewSocket(pc)
	assert.NoError(t, s.SetReadDeadline(time.Now()))
	_, _, err := s.ReadFrom(nil)
	assert.ErrorIs(t, err, ErrTimeout{})
	assert.NoError(t, s.SetWriteDeadline(time.Now()))
	_, err = s.WriteTo(nil, nil)
	assert.ErrorIs(t, err, ErrTimeout{})
	assert.NoError(t, s.SetDeadline(time.Time{}))
	assert.NoError(t, s.Close())
}

func TestSaturateSocketConnIDs(t *testing.T) {
	s := NewSocket(newTestUDP(t))
	var acceptedConns, dialedConns []net.Conn
	for i := 0; i < 500; i++ {
		accepted := make(chan struct{})
		go func() {
			c, err := s.Accept()
			if err != nil {
				t.Log(err)
				return
			}
			acceptedConns = append(acceptedConns, c)
			close(accepted)
		}()
		c, err := s.DialContext(ctx, s.Addr())
		require.NoError(t, err)
		dialedConns = append(dialedConns, c)
		<-accepted
	}
	t.Logf("%d dialed conns, %d accepted", len(dialedConns), len(acceptedConns))
	for i := range dialedConns {
		data := []byte(fmt.Sprintf("%7d", i))
		dc := dialedConns[i]
		n, err := dc.Write(data)
		require.NoError(t, err)
		require.EqualValues(t, 7, n)
		require.NoError(t, dc.Close())
		var b [8]byte
		ac := acceptedConns[i]
		n, err = ac.Read(b[:])
		require.NoError(t, err)
		require.EqualValues(t, 7, n)
		require.EqualValues(t, data, b[:n])
		n, err = ac.Read(b[:])
		require.EqualValues(t, 0, n)
		require.EqualValues(t, io.EOF, err)
		ac.Close()
	}
}

func TestUTPRawConn(t *testing.T) {
	ssock := NewSocket(newTestUDP(t))
	go func() {
		for {
			_, err := ssock.Accept()
			if err != nil {
				break
			}
		}
	}()
	// Connect a UTP peer to see if the RawConn will still work.
	utpPeer := func() net.Conn {
		csock := newTestSocket(t, newTestUDP(t))
		ret, err := csock.DialContext(ctx, ssock.Addr())
		require.NoError(t, err)
		return ret
	}()
	defer utpPeer.Close()

	pc := newTestUDP(t)

	msgsReceived := 0
	const N = 500 // How many messages to send.
	readerStopped := make(chan struct{})
	// The reader goroutine.
	go func() {
		defer close(readerStopped)
		b := make([]byte, 500)
		for i := 0; i < N; i++ {
			n, _, err := ssock.ReadFrom(b)
			if err != nil {
				t.Fatalf("error reading from raw conn: %s", err)
			}
			msgsReceived++
			var d int
			fmt.Sscan(string(b[:n]), &d)
			if d != i {
				log.Printf("got wrong number: expected %d, got %d", i, d)
			}
		}
	}()
	udpAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("localhost:%d", missinggo.AddrPort(ssock.LocalAddr())))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < N; i++ {
		_, err := pc.WriteTo([]byte(fmt.Sprintf("%d", i)), udpAddr)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Microsecond)
	}
	select {
	case <-readerStopped:
	case <-time.After(time.Second):
		t.Fatal("reader timed out")
	}
	if msgsReceived != N {
		t.Fatalf("messages received: %d", msgsReceived)
	}
}

func TestAcceptGone(t *testing.T) {
	t.Skip("TODO")

	serveSock := newTestSocket(t, newTestUDP(t))

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	clientSock := newTestSocket(t, newTestUDP(t))
	_, err := clientSock.DialContext(ctx, serveSock.Addr())
	require.Error(t, err)

	// Will succeed because we don't signal that we give up dialing, or check
	// that the handshake is completed before returning the new Conn.
	c, err := serveSock.Accept()
	require.NoError(t, err)
	defer c.Close()
	err = c.SetReadDeadline(time.Now().Add(time.Millisecond))
	require.NoError(t, err)
	_, err = c.Read(nil)
	require.EqualError(t, err, "i/o timeout")
}
