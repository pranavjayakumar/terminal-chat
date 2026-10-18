//go:build voice

package voice

/*
#cgo LDFLAGS: -framework AudioToolbox -framework CoreAudio
#include "vpio_darwin.h"
*/
import "C"

import (
	"fmt"
	"runtime/cgo"
	"sync"
	"unsafe"
)

// vpio is a duplex on macOS's voice-processing I/O unit (vpio_darwin.c).
type vpio struct {
	v    *C.tc_vpio
	h    cgo.Handle
	once sync.Once
}

func openVPIO(cb func(out, in []int16)) (Duplex, error) {
	h := cgo.NewHandle(cb)
	var st C.int32_t
	v := C.tc_vpio_open(C.uintptr_t(h), &st)
	if v == nil {
		h.Delete()
		return nil, fmt.Errorf("voice processing unit: OSStatus %d", int32(st))
	}
	return &vpio{v: v, h: h}, nil
}

func (d *vpio) SystemAEC() bool { return true }

func (d *vpio) Close() error {
	d.once.Do(func() {
		C.tc_vpio_close(d.v) // stops and disposes the unit: no more callbacks
		d.h.Delete()
	})
	return nil
}

//export tcVPIOCallback
func tcVPIOCallback(h C.uintptr_t, out, in *C.int16_t, n C.uint32_t) {
	cb := cgo.Handle(h).Value().(func(out, in []int16))
	o := unsafe.Slice((*int16)(unsafe.Pointer(out)), int(n))
	i := unsafe.Slice((*int16)(unsafe.Pointer(in)), int(n))
	cb(o, i)
}
