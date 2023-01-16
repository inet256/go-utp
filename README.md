# Go μTP
[![GoDoc](https://godoc.org/github.com/inet256/go-utp?status.svg)](https://godoc.org/github.com/inet256/go-utp)

Go implementation of the [Micro Transport Protocol](https://en.wikipedia.org/wiki/Micro_Transport_Protocol).

## Getting Started
First you will need a `net.PacketConn`.
This can be anything implementing the interface.
There are no abstraction-breaking checks for a particular implementation.
Then use `utp.NewSocket` to get started.

```go
func createSocket() *utp.Socket {
    // Create a UDP socket listening on all addresses
    pc, err := net.ListenPacket("udp", "0.0.0.0:0")
    if err != nil {
        panic(err)
    }

    // Call utp.New to create a *utp.Socket from the net.PacketConn
    s := utp.NewSocket(pc)
    return s
}

// As a server
func useAsServer(s *utp.Socket) error {
    for {
        conn, err := s.Accept()
        go func() {
            defer conn.Close()
            // handle the conn
        }
    }
}

// As a client
func useAsClient(s *utp.Socket, raddr net.Addr) error {
    conn, err := s.DialContext(ctx, raddr)
    if err != nil {
        return err
    }
    defer conn.Close
    // send some data to the other party
    _, err := conn.Write([]byte("hello world"))
    return err
}
```

### Instrumentation
All logging and metrics are provided by the [stdctx](https://github.com/brendoncarroll/stdctx) library.
If you want logs or metrics just configure a context as described there, and pass it in.

## Contributing
`just` is used to run commands.

`just test` runs the tests.

Contributions are welcome.

## Roadmap
Long term it would be best for the implementation to change to be more testable.
The `Socket` and `Conn` objects are the right API (adhearing to `net.Listener`/`net.Dialer` and `net.Conn` respectively), but they are difficult to test.
There needs to be a state machine object, which doesn't have go routines or timers internally, so that arbitrary stimulus at arbitrary states can be tested.

## Fork
Orginally forked from `github.com/anacrolix/utp`.
There were a number of reasons for forking:
- The pure go implementation is deprecated in favor of a C-Go port of libutp.  This fork will continue to be pure Go.
- The API was very UDP specifc, taking string addresses in several places.
- Legacy logging, and instrumentation.
- High quality user experience on top of INET256.
