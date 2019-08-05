// Copyright 2019 Xu Xu. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.

// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package eviop

import (
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// Action is an action that occurs after the completion of an event.
type Action int

const (
	// None indicates that no action should occur following an event.
	None Action = iota
	// Detach detaches a connection. Not available for UDP connections.
	Detach
	// Close closes the connection.
	Close
	// Shutdown shutdowns the server.
	Shutdown
)

// Options are set when the client opens.
type Options struct {
	// TCPKeepAlive (SO_KEEPALIVE) socket option.
	TCPKeepAlive time.Duration
}

// Server represents a server context which provides information about the
// running server and has control functions for managing state.
type Server struct {
	// The addrs parameter is an array of listening addresses that align
	// with the addr strings passed to the Serve function.
	Addrs []net.Addr
	// NumLoops is the number of loops that the server is using.
	NumLoops int
}

// LoadBalance sets the load balancing method.
type LoadBalance int

const (
	// Random requests that connections are randomly distributed.
	Random LoadBalance = iota
	// RoundRobin requests that connections are distributed to a loop in a
	// round-robin fashion.
	RoundRobin
	// LeastConnections assigns the next accepted connection to the loop with
	// the least number of active connections.
	LeastConnections
)

// Events represents the server events for the Serve call.
// Each event has an Action return value that is used manage the state
// of the connection and server.
type Events struct {
	// NumLoops sets the number of loops to use for the server. Setting this
	// to a value greater than 1 will effectively make the server
	// multithreaded for multi-core machines. Which means you must take care
	// with synchonizing memory between all event callbacks. Setting to 0 or 1
	// will run the server single-threaded. Setting to -1 will automatically
	// assign this value equal to runtime.NumProcs().
	NumLoops int
	// LoadBalance sets the load balancing method. Load balancing is always a
	// best effort to attempt to distribute the incoming connections between
	// multiple loops. This option is only works when NumLoops is set.
	LoadBalance LoadBalance
	// Serving fires when the server can accept connections. The server
	// parameter has information and various utilities.
	Serving func(server Server) (action Action)
	// Opened fires when a new connection has opened.
	// The info parameter has information about the connection such as
	// it's local and remote address.
	// Use the out return value to write data to the connection.
	// The opts return value is used to set connection options.
	Opened func(c *Conn) (opts Options, action Action)
	// Closed fires when a connection has closed.
	// The err parameter is the last known connection error.
	Closed func(c *Conn, err error) (action Action)
	// Detached fires when a connection has been previously detached.
	// Once detached it's up to the receiver of this event to manage the
	// state of the connection. The Closed event will not be called for
	// this connection.
	// The conn parameter is a ReadWriteCloser that represents the
	// underlying socket connection. It can be freely used in goroutines
	// and should be closed when it's no longer needed.
	Detached func(c *Conn, rwc io.ReadWriteCloser) (action Action)
	// PreWrite fires just before any data is written to any client socket.
	PreWrite func()
	// Data fires when a connection sends the server data.
	// The in parameter is the incoming data.
	// Use the out return value to write data to the connection.
	Data func(c *Conn) (action Action)
	// Tick fires immediately after the server starts and will fire again
	// following the duration specified by the delay return value.
	Tick func() (delay time.Duration, action Action)
}

// Serve starts handling events for the specified addresses.
//
// Addresses should use a scheme prefix and be formatted
// like `tcp://192.168.0.10:9851` or `unix://socket`.
// Valid network schemes:
//  tcp   - bind to both IPv4 and IPv6
//  tcp4  - IPv4
//  tcp6  - IPv6
//  udp   - bind to both IPv4 and IPv6
//  udp4  - IPv4
//  udp6  - IPv6
//  unix  - Unix Domain Socket
//
// The "tcp" network scheme is assumed when one is not specified.
func Serve(events Events, waitTimeout time.Duration, addr ...string) error {
	var lns []*listener
	defer func() {
		for _, ln := range lns {
			ln.close()
		}
	}()

	for _, addr := range addr {
		var ln listener

		ln.network, ln.addr, ln.opts = parseAddr(addr)
		if ln.network == "unix" {
			os.RemoveAll(ln.addr)
		}
		var err error
		if ln.network == "udp" {
			if ln.opts.reusePort {
				ln.pconn, err = reuseportListenPacket(ln.network, ln.addr)
			} else {
				ln.pconn, err = net.ListenPacket(ln.network, ln.addr)
			}
		} else {
			if ln.opts.reusePort {
				ln.ln, err = reuseportListen(ln.network, ln.addr)
			} else {
				ln.ln, err = net.Listen(ln.network, ln.addr)
			}
		}
		if err != nil {
			return err
		}
		if ln.pconn != nil {
			ln.lnaddr = ln.pconn.LocalAddr()
		} else {
			ln.lnaddr = ln.ln.Addr()
		}

		if err := ln.system(); err != nil {
			return err
		}

		lns = append(lns, &ln)
	}
	return serve(events, waitTimeout, lns)
}

// InputStream is a helper type for managing input streams from inside
// the Data event.
type InputStream struct{ b []byte }

// Begin accepts a new packet and returns a working sequence of
// unprocessed bytes.
func (is *InputStream) Begin(packet []byte) (data []byte) {
	data = packet
	if len(is.b) > 0 {
		is.b = append(is.b, data...)
		data = is.b
	}
	return data
}

// End shifts the stream to match the unprocessed data.
func (is *InputStream) End(data []byte) {
	if len(data) > 0 {
		if len(data) != len(is.b) {
			is.b = append(is.b[:0], data...)
		}
	} else if len(is.b) > 0 {
		is.b = is.b[:0]
	}
}

type listener struct {
	ln      net.Listener
	lnaddr  net.Addr
	pconn   net.PacketConn
	opts    addrOpts
	f       *os.File
	fd      int
	network string
	addr    string
}

type addrOpts struct {
	reusePort bool
}

func parseAddr(addr string) (network, address string, opts addrOpts) {
	network = "tcp"
	address = addr
	opts.reusePort = false
	if strings.Contains(address, "://") {
		network = strings.Split(address, "://")[0]
		address = strings.Split(address, "://")[1]
	}
	q := strings.Index(address, "?")
	if q != -1 {
		for _, part := range strings.Split(address[q+1:], "&") {
			kv := strings.Split(part, "=")
			if len(kv) == 2 {
				switch kv[0] {
				case "reuseport":
					if len(kv[1]) != 0 {
						switch kv[1][0] {
						default:
							opts.reusePort = kv[1][0] >= '1' && kv[1][0] <= '9'
						case 'T', 't', 'Y', 'y':
							opts.reusePort = true
						}
					}
				}
			}
		}
		address = address[:q]
	}
	return
}
