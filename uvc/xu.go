package uvc

/*
#cgo pkg-config: libusb-1.0
#include <libusb.h>
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"io"
	"unsafe"
)

// The camera's only vendor control: extension unit 10, GUID
// a29e7641-de04-47e3-8b2b-f4341aff003b, advertising selector 1. TDView
// presumably drives the camera through it.
const (
	XUUnit     = 10
	XUSelector = 1
	getRes     = 0x84
)

// unitControl issues a class request to a unit or terminal on the control
// interface.
func (s *Stream) unitControl(req C.uint8_t, unit, selector int, data []byte) (int, error) {
	reqType := C.uint8_t(0x21)
	if req&0x80 != 0 {
		reqType = 0xA1
	}
	rc := C.libusb_control_transfer(s.h, reqType, req, C.uint16_t(selector<<8), C.uint16_t(unit<<8|ifControl),
		(*C.uchar)(unsafe.Pointer(&data[0])), C.uint16_t(len(data)), 1000)
	return int(rc), check(rc)
}

// DumpUnitControl prints the answers to every GET request for one control.
// Read-only: nothing is written to the camera.
func (s *Stream) DumpUnitControl(w io.Writer, unit, selector int) {
	info := make([]byte, 1)
	if _, err := s.unitControl(getInfo, unit, selector, info); err != nil {
		fmt.Fprintf(w, "unit %d selector %d: GET_INFO %v\n", unit, selector, err)
		return
	}
	size := make([]byte, 2)
	n := 0
	if _, err := s.unitControl(getLen, unit, selector, size); err == nil {
		n = int(binary.LittleEndian.Uint16(size))
	}
	fmt.Fprintf(w, "unit %d selector %d: info %#02x (get %v, set %v, disabled %v, autoupdate %v, async %v) len %d\n",
		unit, selector, info[0], info[0]&1 != 0, info[0]&2 != 0, info[0]&4 != 0, info[0]&8 != 0, info[0]&16 != 0, n)
	if n == 0 {
		n = 64
	}
	for _, q := range []struct {
		name string
		req  C.uint8_t
	}{{"CUR", getCur}, {"MIN", getMin}, {"MAX", getMax}, {"RES", getRes}, {"DEF", getDef}} {
		b := make([]byte, n)
		got, err := s.unitControl(q.req, unit, selector, b)
		if err != nil {
			fmt.Fprintf(w, "  %s: %v\n", q.name, err)
			continue
		}
		fmt.Fprintf(w, "  %s: % x\n", q.name, b[:got])
	}
}

// GetXU reads the current value of a vendor extension unit control.
func (s *Stream) GetXU(selector int) ([]byte, error) {
	b := make([]byte, 4)
	n, err := s.unitControl(getCur, XUUnit, selector, b)
	if err != nil {
		return nil, err
	}
	return b[:n], nil
}

// SetXU writes a vendor extension unit control.
func (s *Stream) SetXU(selector int, data []byte) error {
	_, err := s.unitControl(setCur, XUUnit, selector, data)
	return err
}
