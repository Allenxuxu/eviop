// Copyright 2019 Xu Xu. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.

// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build !darwin,!netbsd,!freebsd,!openbsd,!dragonfly,!linux

package eviop

import (
	"errors"
	"net"
	"os"
	"time"
)

func (ln *listener) close() {
	if ln.ln != nil {
		ln.ln.Close()
	}
	if ln.pconn != nil {
		ln.pconn.Close()
	}
	if ln.network == "unix" {
		os.RemoveAll(ln.addr)
	}
}

func (ln *listener) system() error {
	return nil
}

func serve(events Events, waitTimeout time.Duration, listeners []*listener) error {
	return stdserve(events, waitTimeout, listeners)
}

func reuseportListenPacket(proto, addr string) (l net.PacketConn, err error) {
	return nil, errors.New("reuseport is not available")
}

func reuseportListen(proto, addr string) (l net.Listener, err error) {
	return nil, errors.New("reuseport is not available")
}
