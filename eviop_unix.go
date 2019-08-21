// Copyright 2019 Xu Xu. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.

// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build darwin netbsd freebsd openbsd dragonfly linux

package eviop

import (
	"errors"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Allenxuxu/eviop/internal"
	"github.com/Allenxuxu/ringbuffer"
	"github.com/RussellLuo/timingwheel"
	reuseport "github.com/kavu/go_reuseport"
)

var errClosing = errors.New("closing")

//var errCloseConns = errors.New("close conns")

type server struct {
	events      Events             // user events
	loops       []*loop            // all the loops
	lns         []*listener        // all the listeners
	wg          sync.WaitGroup     // loop close waitgroup
	cond        *sync.Cond         // shutdown signaler
	balance     LoadBalance        // load balancing method
	accepted    uintptr            // accept counter
	tch         chan time.Duration // ticker channel
	waitTimeout time.Duration

	//ticktm   time.Time      // next tick time
}

type loop struct {
	idx     int            // loop index in the server loops list
	poll    *internal.Poll // epoll or kqueue
	packet  []byte         // read packet buffer
	fdconns map[int]*Conn  // loop connections fd -> conn
	count   int32          // connection count
	tw      *timingwheel.TimingWheel
}

// waitForShutdown waits for a signal to shutdown
func (s *server) waitForShutdown() {
	s.cond.L.Lock()
	s.cond.Wait()
	s.cond.L.Unlock()
}

// signalShutdown signals a shutdown an begins server closing
func (s *server) signalShutdown() {
	s.cond.L.Lock()
	s.cond.Signal()
	s.cond.L.Unlock()
}

func serve(events Events, waitTimeout time.Duration, listeners []*listener) error {
	// figure out the correct number of loops/goroutines to use.
	numLoops := events.NumLoops
	if numLoops <= 0 {
		if numLoops == 0 {
			numLoops = 1
		} else {
			numLoops = runtime.NumCPU()
		}
	}

	s := &server{}
	s.events = events
	s.lns = listeners
	s.cond = sync.NewCond(&sync.Mutex{})
	s.balance = events.LoadBalance
	s.tch = make(chan time.Duration)
	s.waitTimeout = waitTimeout

	//println("-- server starting")
	if s.events.Serving != nil {
		var svr Server
		svr.NumLoops = numLoops
		svr.Addrs = make([]net.Addr, len(listeners))
		for i, ln := range listeners {
			svr.Addrs[i] = ln.lnaddr
		}
		action := s.events.Serving(svr)
		switch action {
		case None:
		case Shutdown:
			return nil
		}
	}

	defer func() {
		// wait on a signal for shutdown
		s.waitForShutdown()

		// notify all loops to close by closing all listeners
		for _, l := range s.loops {
			_ = l.poll.Trigger(errClosing)
		}

		// wait on all loops to complete reading events
		s.wg.Wait()

		// close loops and all outstanding connections
		for _, l := range s.loops {
			for _, c := range l.fdconns {
				_ = loopCloseConn(s, l, c, nil)
			}
			_ = l.poll.Close()
			l.tw.Stop()
		}
		//println("-- server stopped")
	}()

	// create loops locally and bind the listeners.
	for i := 0; i < numLoops; i++ {
		l := &loop{
			idx:     i,
			poll:    internal.OpenPoll(),
			packet:  make([]byte, 0xFFFF),
			fdconns: make(map[int]*Conn),
			tw:      timingwheel.NewTimingWheel(time.Second, 60),
		}
		l.tw.Start()
		for _, ln := range listeners {
			l.poll.AddRead(ln.fd)
		}
		s.loops = append(s.loops, l)
	}
	// start loops in background
	s.wg.Add(len(s.loops))
	for _, l := range s.loops {
		go loopRun(s, l)
	}
	return nil
}

func closeTimeoutConn(s *server, l *loop, c *Conn) func() {
	return func() {
		now := time.Now()
		intervals := now.Sub(c.getActiveTime())
		if intervals >= s.waitTimeout {
			c.toClose.Set(true)
			c.Wake()
		} else {
			l.tw.AfterFunc(s.waitTimeout-intervals, closeTimeoutConn(s, l, c))
		}
	}
}

func loopCloseConn(s *server, l *loop, c *Conn, err error) error {
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

func loopNote(s *server, l *loop, note interface{}) error {
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
		return loopWake(s, l, v)
	}
	return err
}

func loopRun(s *server, l *loop) {
	defer func() {
		//fmt.Println("-- loop stopped --", l.idx)
		s.signalShutdown()
		s.wg.Done()
	}()

	if l.idx == 0 && s.events.Tick != nil {
		go loopTicker(s, l)
	}

	//fmt.Println("-- loop started --", l.idx)
	_ = l.poll.Wait(func(fd int, note interface{}) error {
		if fd == 0 {
			return loopNote(s, l, note)
		}
		c := l.fdconns[fd]
		switch {
		case c == nil:
			return loopAccept(s, l, fd)
		case !c.opened:
			return loopOpened(s, l, c)
		case c.outBuffer.Length() > 0:
			return loopWrite(s, l, c)
		default:
			return loopRead(s, l, c)
		}
	})
}

func loopTicker(s *server, l *loop) {
	for {
		if err := l.poll.Trigger(time.Duration(0)); err != nil {
			break
		}
		time.Sleep(<-s.tch)
	}
}

func loopAccept(s *server, l *loop, fd int) error {
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
				return loopUDPRead(s, l, i, fd)
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
				l.tw.AfterFunc(s.waitTimeout, closeTimeoutConn(s, l, c))
			}

			break
		}
	}
	return nil
}

func loopUDPRead(s *server, l *loop, lnidx, fd int) error {
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

func loopOpened(s *server, l *loop, c *Conn) error {
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
	return handlerAction(s, l, c)
}

func loopWrite(s *server, l *loop, c *Conn) error {
	if s.events.PreWrite != nil {
		s.events.PreWrite()
	}

	first, end := c.outBuffer.PeekAll()
	n, err := syscall.Write(c.fd, first)
	if err != nil {
		if err == syscall.EAGAIN {
			return nil
		}
		return loopCloseConn(s, l, c, err)
	}
	c.outBuffer.Retrieve(n)

	if n == len(first) && len(end) > 0 {
		n, err := syscall.Write(c.fd, end)
		if err != nil {
			if err == syscall.EAGAIN {
				return nil
			}
			return loopCloseConn(s, l, c, err)
		}
		c.outBuffer.Retrieve(n)
	}

	if c.outBuffer.Length() == 0 {
		l.poll.ModRead(c.fd)
	}
	return nil
}

func handlerAction(s *server, l *loop, c *Conn) error {
	switch c.action {
	default:
		c.action = None
	case Close:
		return loopCloseConn(s, l, c, nil)
	case Shutdown:
		return errClosing
	}
	return nil
}

func loopWake(s *server, l *loop, c *Conn) error {
	if c.toClose.Get() {
		_ = loopCloseConn(s, l, c, nil)
		return nil
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
	return handlerAction(s, l, c)
}

func loopRead(s *server, l *loop, c *Conn) error {
	n, err := syscall.Read(c.fd, l.packet)
	if n == 0 || err != nil {
		if err == syscall.EAGAIN {
			return nil
		}
		return loopCloseConn(s, l, c, err)
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
	return handlerAction(s, l, c)
}

func (ln *listener) close() {
	if ln.fd != 0 {
		_ = syscall.Close(ln.fd)
	}
	if ln.f != nil {
		_ = ln.f.Close()
	}
	if ln.ln != nil {
		_ = ln.ln.Close()
	}
	if ln.pconn != nil {
		_ = ln.pconn.Close()
	}
	if ln.network == "unix" {
		_ = os.RemoveAll(ln.addr)
	}
}

// system takes the net listener and detaches it from it's parent
// event loop, grabs the file descriptor, and makes it non-blocking.
func (ln *listener) system() error {
	var err error
	switch netln := ln.ln.(type) {
	case nil:
		switch pconn := ln.pconn.(type) {
		case *net.UDPConn:
			ln.f, err = pconn.File()
		}
	case *net.TCPListener:
		ln.f, err = netln.File()
	case *net.UnixListener:
		ln.f, err = netln.File()
	}
	if err != nil {
		ln.close()
		return err
	}
	ln.fd = int(ln.f.Fd())
	return syscall.SetNonblock(ln.fd, true)
}

func reuseportListenPacket(proto, addr string) (l net.PacketConn, err error) {
	return reuseport.ListenPacket(proto, addr)
}

func reuseportListen(proto, addr string) (l net.Listener, err error) {
	return reuseport.Listen(proto, addr)
}
