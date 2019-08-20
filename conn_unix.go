// Copyright 2019 Xu Xu. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.

// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build darwin netbsd freebsd openbsd dragonfly linux

package eviop

import (
	"net"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Allenxuxu/ringbuffer"
)

type Conn struct {
	fd         int                    // file descriptor
	lnidx      int                    // listener index in the server lns list
	outBuffer  *ringbuffer.RingBuffer // write buffer
	inBuffer   *ringbuffer.RingBuffer
	sa         syscall.Sockaddr // remote socket address
	opened     bool             // connection opened event fired
	toClose    AtomicBool
	action     Action      // next user action
	ctx        interface{} // user-defined context
	addrIndex  int         // index of listening address
	localAddr  net.Addr    // local addre
	remoteAddr net.Addr    // remote addr
	activeTime int64       // Last received message time
	loop       *loop       // connected loop
}

func (c *Conn) Send(buf []byte) {
	n, err := syscall.Write(c.fd, buf)
	if err != nil {
		_, _ = c.outBuffer.Write(buf)
		return
	}

	if n < len(buf) {
		_, _ = c.outBuffer.Write(buf[n:])
	}
}
func (c *Conn) Read(buf []byte) (int, error) { return c.inBuffer.Read(buf) }
func (c *Conn) ReadAll() []byte {
	ret := c.inBuffer.Bytes()
	c.inBuffer.RetrieveAll()
	return ret
}
func (c *Conn) ReadLength() int             { return c.inBuffer.Length() }
func (c *Conn) Peek(n int) ([]byte, []byte) { return c.inBuffer.Peek(n) }
func (c *Conn) PeekAll() ([]byte, []byte)   { return c.inBuffer.PeekAll() }
func (c *Conn) Retrieve(n int)              { c.inBuffer.Retrieve(n) }
func (c *Conn) RetrieveAll()                { c.inBuffer.RetrieveAll() }

func (c *Conn) Context() interface{}       { return c.ctx }
func (c *Conn) SetContext(ctx interface{}) { c.ctx = ctx }
func (c *Conn) AddrIndex() int             { return c.addrIndex }
func (c *Conn) LocalAddr() net.Addr        { return c.localAddr }
func (c *Conn) RemoteAddr() net.Addr       { return c.remoteAddr }
func (c *Conn) setActiveTime(t time.Time)  { atomic.SwapInt64(&c.activeTime, t.Unix()) }
func (c *Conn) getActiveTime() time.Time   { return time.Unix(atomic.LoadInt64(&c.activeTime), 0) }
func (c *Conn) Wake() {
	if c.loop != nil {
		_ = c.loop.poll.Trigger(c)
	}
}
