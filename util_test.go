// Copyright 2019 Xu Xu. All rights reserved.

// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package eviop

import "testing"

func TestAtomicBool(t *testing.T) {

	t.Run("AtomicBool", func(t *testing.T) {
		var isOk AtomicBool
		if isOk.Get() != false {
			t.Fatal("expect false")
		}

		isOk.Set(true)
		if isOk.Get() != true {
			t.Fatal("expect true")
		}

		isOk.Set(false)
		if isOk.Get() != false {
			t.Fatal("expect false")
		}
	})
}
