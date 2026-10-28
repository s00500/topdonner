package uvc

import (
	"bytes"
	"encoding/binary"
)

// Whatever format is negotiated, the TC004 interleaves two frame kinds with
// clean FID/EOF framing:
//
//   - thermal: 10-byte marker FF 00 FF 00 FF 00 FF 00 FF 00, then
//     ThermalW*ThermalH uint16 LE temperatures in Kelvin x10, row-major,
//     portrait like the camera's .irg files; ~4.5 new frames per second
//   - video:   the camera's rendered picture stretched to 16:9, MJPEG at
//     ~24 fps once negotiated properly, otherwise 1280x720 YUV 4:2:0
const (
	ThermalW = 180
	ThermalH = 240
)

// UVC payload header bits (bmHeaderInfo).
const (
	hdrFID = 1 << 0
	hdrEOF = 1 << 1
)

var thermalMarker = []byte{0xFF, 0x00, 0xFF, 0x00, 0xFF, 0x00, 0xFF, 0x00, 0xFF, 0x00}

const thermalFrameSize = 10 + ThermalW*ThermalH*2

// Assembler joins payloads into frames on FID toggles and EOF flags.
type Assembler struct {
	buf     []byte
	fid     int
	OnFrame func(frame []byte) // frame is only valid during the call
}

func NewAssembler(onFrame func([]byte)) *Assembler {
	return &Assembler{fid: -1, OnFrame: onFrame}
}

func (a *Assembler) Push(p []byte) {
	hlen := int(p[0])
	if hlen < 2 || hlen > len(p) {
		return
	}
	info := p[1]
	fid := int(info & hdrFID)
	if fid != a.fid && len(a.buf) > 0 {
		a.emit()
	}
	a.fid = fid
	a.buf = append(a.buf, p[hlen:]...)
	if info&hdrEOF != 0 {
		a.emit()
	}
}

func (a *Assembler) emit() {
	if a.OnFrame != nil {
		a.OnFrame(a.buf)
	}
	a.buf = a.buf[:0]
}

// ThermalFrame returns the raw Kelvin x10 values if frame is a thermal frame.
func ThermalFrame(frame []byte) ([]uint16, bool) {
	if len(frame) != thermalFrameSize || !bytes.HasPrefix(frame, thermalMarker) {
		return nil, false
	}
	k := make([]uint16, ThermalW*ThermalH)
	for i := range k {
		k[i] = binary.LittleEndian.Uint16(frame[10+2*i:])
	}
	return k, true
}
