package prober

import (
	"context"
	"io"
	"net"
	"sync"
)

// boundedConn is a net.Conn whose reads stop, with io.EOF, after a total of limit bytes (sec-
// hardening-2 S8, review of iter-0212 P2-1). It bounds what a protocol driver reads from a probed
// peer when the driver itself offers no total bound: the driver sees a peer that closed, and the
// probe ends at the bound instead of its deadline or a gigabyte later.
type boundedConn struct {
	net.Conn
	mu   sync.Mutex
	left int64
}

func (c *boundedConn) Read(p []byte) (int, error) {
	// Reserve before the (blocking) read, return what it did not use: the lock is never held
	// across the read, and concurrent readers together never exceed the limit.
	c.mu.Lock()
	if c.left <= 0 {
		c.mu.Unlock()
		return 0, io.EOF
	}
	want := int64(len(p))
	if want > c.left {
		want = c.left
	}
	c.left -= want
	c.mu.Unlock()
	n, err := c.Conn.Read(p[:want])
	c.mu.Lock()
	c.left += want - int64(n)
	c.mu.Unlock()
	return n, err
}

// boundedDial wraps every connection dial returns in a boundedConn of limit bytes.
func boundedDial(dial func(context.Context, string, string) (net.Conn, error), limit int64) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &boundedConn{Conn: conn, left: limit}, nil
	}
}
