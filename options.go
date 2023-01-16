package utp

import (
	"context"
	"net"
)

// SocketOption configures a Socket
type SocketOption func(*socketConfig)

type socketConfig struct {
	noOpClose bool
	connConfig
	bgCtx context.Context
}

func WithNoOpClose() SocketOption {
	return func(c *socketConfig) {
		c.noOpClose = true
	}
}

func WithConnOption(o ConnOption) SocketOption {
	return func(c *socketConfig) {
		o(&c.connConfig)
	}
}

// WithBackground sets the background context.
// This can be used to enable instrumentation with the stdctx library.
func WithBackground(ctx context.Context) SocketOption {
	return func(c *socketConfig) {
		c.bgCtx = ctx
	}
}

type packetConnNopCloser struct {
	net.PacketConn
}

func (packetConnNopCloser) Close() error {
	return nil
}

type connConfig struct{}

// ConnOption is used to configure Conn specific options
type ConnOption func(*connConfig)
