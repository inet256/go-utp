package utp

import (
	"errors"
	"net"
)

// TODO: figure out errors
var (
	errClosed               = errors.New("closed")
	errTimeout    net.Error = timeoutError{"i/o timeout"}
	errAckTimeout           = timeoutError{"timed out waiting for ack"}
)

type timeoutError struct {
	msg string
}

func (me timeoutError) Timeout() bool   { return true }
func (me timeoutError) Error() string   { return me.msg }
func (me timeoutError) Temporary() bool { return false }
