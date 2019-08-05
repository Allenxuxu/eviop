// Copyright 2019 Xu Xu. All rights reserved.

// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package eviop

import "sync/atomic"

type AtomicBool struct {
	b int32
}

func (a *AtomicBool) Set(b bool) {
	var newV int32
	if b {
		newV = 1
	}
	atomic.SwapInt32(&a.b, newV)
}

func (a *AtomicBool) Get() bool {
	return atomic.LoadInt32(&a.b) == 1
}
