package utp

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	pprofsync "github.com/anacrolix/sync"
	"github.com/brendoncarroll/stdctx/units"
)

const (
	// Maximum received SYNs that haven't been accepted. If more SYNs are
	// received, a pseudo randomly selected SYN is replied to with a reset to
	// make room.
	backlog = 50

	// IPv6 min MTU is 1280, -40 for IPv6 header, and ~8 for fragment header?
	minMTU = 1438 // Why?

	// uTP header of 20, +2 for the next extension, and an optional selective
	// ACK.
	maxHeaderSize  = 20 + 2 + (((maxUnackedInbound+7)/8)+3)/4*4
	maxPayloadSize = minMTU - maxHeaderSize
	maxRecvSize    = 0x2000

	// Maximum out-of-order packets to buffer.
	maxUnackedInbound = 256
	maxUnackedSends   = 256

	readBufferLen = 1 << 20 // ~1MiB

	// How long to wait before sending a state packet, after one is required.
	// This prevents spamming a state packet for every packet received, and
	// non-state packets that are being sent also fill the role.
	pendingSendStateDelay = 500 * time.Microsecond
)

var (
	sendBufferPool = sync.Pool{
		New: func() interface{} { return make([]byte, minMTU) },
	}
	// This is the latency we assume on new connections. It should be higher
	// than the latency we expect on most connections to prevent excessive
	// resending to peers that take a long time to respond, before we've got a
	// better idea of their actual latency.
	initialLatency time.Duration
	// If a write isn't acked within this period, destroy the connection.
	writeTimeout time.Duration
	// Assume the connection has been closed by the peer getting no packets of
	// any kind for this time.
	packetReadTimeout time.Duration
)

func setDefaultDurations() {
	// An approximate upper bound for most connections across the world.
	initialLatency = 400 * time.Millisecond
	// Getting no reply for this period for a packet, we can probably rule out
	// latency and client lag.
	writeTimeout = 15 * time.Second
	// Somewhere longer than the BitTorrent grace period (90-120s), and less
	// than default TCP reset (4min).
	packetReadTimeout = 2 * time.Minute
}

func init() {
	// TODO: Remove
	setDefaultDurations()
}

type read struct {
	data []byte
	from net.Addr
}

type syn struct {
	seq_nr, conn_id uint16
	addr            net.Addr
}

var (
	// TODO: what the actual fuck?
	mu                         pprofsync.RWMutex
	sockets                    = map[*Socket]struct{}{}
	logLevel                   = 0
	artificialPacketDropChance = 0.0
)

// TODO: remove
func init() {
	logLevel, _ = strconv.Atoi(os.Getenv("GO_UTP_LOGGING"))
	fmt.Sscanf(os.Getenv("GO_UTP_PACKET_DROP"), "%f", &artificialPacketDropChance)
}

type st int

func (me st) String() string {
	switch me {
	case stData:
		return "stData"
	case stFin:
		return "stFin"
	case stState:
		return "stState"
	case stReset:
		return "stReset"
	case stSyn:
		return "stSyn"
	default:
		panic(fmt.Sprintf("%d", me))
	}
}

const (
	stData  st = 0
	stFin   st = 1
	stState st = 2
	stReset st = 3
	stSyn   st = 4

	// Used for validating packet headers.
	stMax = stSyn
)

type recv struct {
	seen bool
	data []byte
	Type st
}

func nowTimestamp() uint32 {
	return uint32(time.Now().UnixNano() / int64(time.Microsecond))
}

func seqLess(a, b uint16) bool {
	if b < 0x8000 {
		return a < b || a >= b-0x8000
	} else {
		return a < b && a >= b-0x8000
	}
}

func telemIncr(ctx context.Context, m string, x any, u units.Unit) {

}

func telemMark(ctx context.Context, m string, x any, u units.Unit) {

}
