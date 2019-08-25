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

	"github.com/Allenxuxu/eviop/internal"
	"github.com/Allenxuxu/ringbuffer"
	"github.com/RussellLuo/timingwheel"
)

type loop struct {
	idx     int            // loop index in the server loops list
	poll    *internal.Poll // epoll or kqueue
	packet  []byte         // read packet buffer
	fdconns map[int]*Conn  // loop connections fd -> conn
	count   int32          // connection count
	tw      *timingwheel.TimingWheel
}

func (l *loop) closeTimeoutConn(s *server, c *Conn) func() {
	return func() {
		now := time.Now()
		intervals := now.Sub(c.getActiveTime())
		if intervals >= s.waitTimeout {
			c.toClose.Set(true)
			c.Wake()
		} else {
			l.tw.AfterFunc(s.waitTimeout-intervals, l.closeTimeoutConn(s, c))
		}
	}
}

func (l *loop) loopCloseConn(s *server, c *Conn, err error) error {
	atomic.AddInt32(&l.count, -1)
	delete(l.fdconns, c.fd)
	_ = syscall.Close(c.fd)
	if s.events.Closed != nil {
		// TODO 关闭前处理未发送数据
		switch s.events.Closed(c, err) {
		case None:
		case Shutdown:
			return errClosing
		}
	}
	return nil
}

func (l *loop) loopNote(s *server, note interface{}) error {
	var err error
	switch v := note.(type) {
	case time.Duration:
		delay, action := s.events.Tick()
		switch action {
		case None:
		case Shutdown:
			err = errClosing
		}
		s.tch <- delay
	case error: // shutdown
		err = v
	case *Conn:
		// Wake called for connection
		if l.fdconns[v.fd] != v {
			return nil // ignore stale wakes
		}
		return l.loopWake(s, v)
	}
	return err
}

func (l *loop) loopRun(s *server) {
	defer func() {
		//fmt.Println("-- loop stopped --", l.idx)
		s.signalShutdown()
		s.wg.Done()
	}()

	if l.idx == 0 && s.events.Tick != nil {
		go l.loopTicker(s)
	}

	//fmt.Println("-- loop started --", l.idx)
	_ = l.poll.Wait(func(fd int, note interface{}) error {
		if fd == 0 {
			return l.loopNote(s, note)
		}
		c := l.fdconns[fd]
		switch {
		case c == nil:
			return l.loopAccept(s, fd)
		case !c.opened:
			return l.loopOpened(s, c)
		case c.outBuffer.Length() > 0:
			return l.loopWrite(s, c)
		default:
			return l.loopRead(s, c)
		}
	})
}

func (l *loop) loopTicker(s *server) {
	for {
		if err := l.poll.Trigger(time.Duration(0)); err != nil {
			break
		}
		time.Sleep(<-s.tch)
	}
}

func (l *loop) loopAccept(s *server, fd int) error {
	for i, ln := range s.lns {
		if ln.fd == fd {
			if len(s.loops) > 1 {
				switch s.balance {
				case LeastConnections:
					n := atomic.LoadInt32(&l.count)
					for _, lp := range s.loops {
						if lp.idx != l.idx {
							if atomic.LoadInt32(&lp.count) < n {
								return nil // do not accept
							}
						}
					}
				case RoundRobin:
					idx := int(atomic.LoadUintptr(&s.accepted)) % len(s.loops)
					if idx != l.idx {
						return nil // do not accept
					}
					atomic.AddUintptr(&s.accepted, 1)
				}
			}
			if ln.pconn != nil {
				return l.loopUDPRead(s, i, fd)
			}
			nfd, sa, err := syscall.Accept(fd)
			if err != nil {
				if err == syscall.EAGAIN {
					return nil
				}
				return err
			}
			if err := syscall.SetNonblock(nfd, true); err != nil {
				return err
			}
			c := &Conn{
				fd:        nfd,
				sa:        sa,
				lnidx:     i,
				outBuffer: ringbuffer.New(1024),
				inBuffer:  ringbuffer.New(1024),
				loop:      l,
			}
			c.setActiveTime(time.Now())
			l.fdconns[c.fd] = c
			l.poll.AddReadWrite(c.fd)
			atomic.AddInt32(&l.count, 1)

			if s.waitTimeout > 0 {
				l.tw.AfterFunc(s.waitTimeout, l.closeTimeoutConn(s, c))
			}

			break
		}
	}
	return nil
}

func (l *loop) loopUDPRead(s *server, lnidx, fd int) error {
	n, sa, err := syscall.Recvfrom(fd, l.packet, 0)
	if err != nil || n == 0 {
		return nil
	}
	if s.events.Data != nil {
		var sa6 syscall.SockaddrInet6
		switch sa := sa.(type) {
		case *syscall.SockaddrInet4:
			sa6.ZoneId = 0
			sa6.Port = sa.Port
			for i := 0; i < 12; i++ {
				sa6.Addr[i] = 0
			}
			sa6.Addr[12] = sa.Addr[0]
			sa6.Addr[13] = sa.Addr[1]
			sa6.Addr[14] = sa.Addr[2]
			sa6.Addr[15] = sa.Addr[3]
		case *syscall.SockaddrInet6:
			sa6 = *sa
		}
		c := &Conn{
			//outBuffer: ringbuffer.New(1024),
			inBuffer: ringbuffer.New(1024),
		}
		c.addrIndex = lnidx
		c.localAddr = s.lns[lnidx].lnaddr
		c.remoteAddr = internal.SockaddrToAddr(&sa6)

		_, _ = c.inBuffer.Write(l.packet[:n])
		out, action := s.events.Data(c, c.inBuffer)
		if len(out) > 0 {
			if s.events.PreWrite != nil {
				s.events.PreWrite()
			}
			_ = syscall.Sendto(fd, out, 0, sa)
		}
		switch action {
		case Shutdown:
			return errClosing
		}
	}
	return nil
}

func (l *loop) loopOpened(s *server, c *Conn) error {
	c.opened = true
	c.addrIndex = c.lnidx
	c.localAddr = s.lns[c.lnidx].lnaddr
	c.remoteAddr = internal.SockaddrToAddr(c.sa)
	if s.events.Opened != nil {
		var opts Options
		var out []byte
		out, opts, c.action = s.events.Opened(c)
		if opts.TCPKeepAlive > 0 {
			if _, ok := s.lns[c.lnidx].ln.(*net.TCPListener); ok {
				_ = internal.SetKeepAlive(c.fd, int(opts.TCPKeepAlive/time.Second))
			}
		}

		if len(out) > 0 {
			c.send(out)
		}
	}
	if c.outBuffer.Length() == 0 {
		l.poll.ModRead(c.fd)
	}
	return l.handlerAction(s, c)
}

func (l *loop) loopWrite(s *server, c *Conn) error {
	if s.events.PreWrite != nil {
		s.events.PreWrite()
	}

	first, end := c.outBuffer.PeekAll()
	n, err := syscall.Write(c.fd, first)
	if err != nil {
		if err == syscall.EAGAIN {
			return nil
		}
		return l.loopCloseConn(s, c, err)
	}
	c.outBuffer.Retrieve(n)

	if n == len(first) && len(end) > 0 {
		n, err := syscall.Write(c.fd, end)
		if err != nil {
			if err == syscall.EAGAIN {
				return nil
			}
			return l.loopCloseConn(s, c, err)
		}
		c.outBuffer.Retrieve(n)
	}

	if c.outBuffer.Length() == 0 {
		l.poll.ModRead(c.fd)
	}
	return nil
}

func (l *loop) handlerAction(s *server, c *Conn) error {
	switch c.action {
	default:
		c.action = None
	case Close:
		return l.loopCloseConn(s, c, nil)
	case Shutdown:
		return errClosing
	}
	return nil
}

func (l *loop) loopWake(s *server, c *Conn) error {
	if c.toClose.Get() {
		_ = l.loopCloseConn(s, c, nil)
		return nil
	}

	var pf []func()
	c.mu.Lock()
	pf = c.pendingFunc
	c.pendingFunc = []func(){}
	c.mu.Unlock()

	for _, f := range pf {
		f()
	}

	if s.events.Data != nil {
		var out []byte
		out, c.action = s.events.Data(c, c.inBuffer)
		if len(out) > 0 {
			c.send(out)
		}

		if c.outBuffer.Length() != 0 {
			l.poll.ModReadWrite(c.fd)
		}
	}
	return l.handlerAction(s, c)
}

func (l *loop) loopRead(s *server, c *Conn) error {
	n, err := syscall.Read(c.fd, l.packet)
	if n == 0 || err != nil {
		if err == syscall.EAGAIN {
			return nil
		}
		return l.loopCloseConn(s, c, err)
	}

	c.setActiveTime(time.Now())

	_, _ = c.inBuffer.Write(l.packet[:n])
	if s.events.Data != nil {
		var out []byte
		out, c.action = s.events.Data(c, c.inBuffer)

		if len(out) > 0 {
			c.send(out)
		}
	}
	if c.outBuffer.Length() != 0 {
		l.poll.ModReadWrite(c.fd)
	}
	return l.handlerAction(s, c)
}
