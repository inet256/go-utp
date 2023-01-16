package utp

import (
	"io"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

func getTCPConnectionPair(t testing.TB) (c1, c2 net.Conn) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	eg := errgroup.Group{}
	eg.Go(func() (err error) {
		c1, err = l.Accept()
		return err
	})
	eg.Go(func() (err error) {
		c2, err = net.Dial("tcp", l.Addr().String())
		return err
	})
	require.NoError(t, eg.Wait())
	return c1, c2
}

func getUTPConnectionPair(t testing.TB) (c1, c2 net.Conn) {
	s1, s2 := newTestSocket(t, newTestUDP(t)), newTestSocket(t, newTestUDP(t))

	eg := errgroup.Group{}
	eg.Go(func() error {
		var err error
		c1, err = s1.Accept()
		return err
	})
	eg.Go(func() error {
		var err error
		c2, err = s2.DialContext(ctx, s1.LocalAddr())
		return err
	})
	return c1, c2
}

func requireWriteAll(t testing.TB, b []byte, w io.Writer) {
	n, err := w.Write(b)
	require.NoError(t, err)
	require.EqualValues(t, len(b), n)
}

func requireReadExactly(t testing.TB, b []byte, r io.Reader) {
	n, err := io.ReadFull(r, b)
	require.NoError(t, err)
	require.EqualValues(t, len(b), n)
}

func benchConnPair(b *testing.B, c0, c1 net.Conn) {
	b.ReportAllocs()
	request := make([]byte, 52)
	response := make([]byte, (128<<10)+8)
	b.SetBytes(int64(len(request) + len(response)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, s := func() (net.Conn, net.Conn) {
			if i%2 == 0 {
				return c0, c1
			} else {
				return c1, c0
			}
		}()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			requireWriteAll(b, request, c)
			requireReadExactly(b, response[:8], c)
			requireReadExactly(b, response[8:], c)
		}()
		go func() {
			defer wg.Done()
			requireReadExactly(b, request[:8], s)
			requireReadExactly(b, request[8:], s)
			requireWriteAll(b, response, s)
		}()
		wg.Wait()
	}
}

func BenchmarkSyncthingTCP(b *testing.B) {
	conn0, conn1 := getTCPConnectionPair(b)
	benchConnPair(b, conn0, conn1)
}

func BenchmarkSyncthingUDPUTP(b *testing.B) {
	conn0, conn1 := getUTPConnectionPair(b)
	benchConnPair(b, conn0, conn1)
}

func BenchmarkSyncthingInprocUTP(b *testing.B) {
	c0, c1 := newTestConnPair(b)
	benchConnPair(b, c0, c1)
}
