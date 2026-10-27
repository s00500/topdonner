package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
)

// IRG is the raw dump format InfiRay-based cameras (Topdon TC004, InfiRay
// C200/P200, Autel drones) write next to their JPGs. Layout, all little-endian:
//
//	0x00 u16  magic (0x0BB0 TC004, 0xACCA C200, 0xABBA other, 0xA004 P200)
//	0x02 u16  header length (0x80 or 0x100); header ends in AC CA
//	0x04 u32  pixel count
//	0x08 u16  height
//	0x0A u16  width
//	0x0C u16  flag0 (C200: 0 => 1/16 K, 1 => 1/10 K)
//	0x1E u32  emissivity x10000
//	0x22 u32  reflected temperature, K x10000 (TC004)
//	0x26 u32  air temperature, K x10000 (TC004)
//	0x2A u32  distance, m x10000
//	hdr       width*height u8 contrast-stretched preview
//	          width*height u16 temperature in Kelvin x scale
//	          optional embedded JPEG until EOF
//
// See github.com/crroush/heatseeker, git.sr.ht/~phlash/irgdump and
// github.com/jaseg/infiray_irg.

var irgModels = map[[2]byte]string{
	{0xB0, 0x0B}: "Topdon TC004",
	{0xCA, 0xAC}: "InfiRay C200",
	{0xBA, 0xAB}: "InfiRay",
	{0x04, 0xA0}: "InfiRay P200",
}

func ReadIRG(path string) (*Thermal, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	t, err := ParseIRG(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

func ParseIRG(b []byte) (*Thermal, error) {
	if len(b) < 0x80 {
		return nil, fmt.Errorf("too short for an IRG file (%d bytes)", len(b))
	}
	u16 := func(o int) int { return int(binary.LittleEndian.Uint16(b[o:])) }
	u32 := func(o int) float64 { return float64(binary.LittleEndian.Uint32(b[o:])) }

	magic := [2]byte{b[0], b[1]}
	model, ok := irgModels[magic]
	if !ok {
		return nil, fmt.Errorf("unknown IRG magic %02x %02x", b[0], b[1])
	}
	hdr := u16(2)
	if hdr < 0x80 || hdr > len(b) {
		return nil, fmt.Errorf("bad header length %#x", hdr)
	}
	if model != "InfiRay P200" && !bytes.Equal(b[hdr-2:hdr], []byte{0xAC, 0xCA}) {
		return nil, fmt.Errorf("header end marker missing at %#x", hdr-2)
	}

	h, w := u16(8), u16(10)
	n := w * h
	if n == 0 || int(u32(4)) != n {
		return nil, fmt.Errorf("resolution %dx%d does not match pixel count %d", w, h, int(u32(4)))
	}
	fine := hdr + n
	end := fine + 2*n
	if end > len(b) {
		return nil, fmt.Errorf("truncated: need %d bytes, have %d", end, len(b))
	}

	scale, zero := 10.0, 273.15
	switch {
	case model == "InfiRay C200" && u16(12) == 0:
		scale = 16
	case model == "InfiRay P200":
		zero = 273.2
	}

	t := &Thermal{
		Model:      model,
		W:          w,
		H:          h,
		C:          make([]float64, n),
		Emissivity: u32(0x1E) / 1e4,
		Distance:   u32(0x2A) / 1e4,
	}
	if model == "Topdon TC004" {
		t.Reflected = u32(0x22)/1e4 - 273.15
		t.Air = u32(0x26)/1e4 - 273.15
	}
	for i := range n {
		t.C[i] = float64(binary.LittleEndian.Uint16(b[fine+2*i:]))/scale - zero
	}
	if rest := b[end:]; bytes.HasPrefix(rest, []byte{0xFF, 0xD8}) {
		t.JPEG = rest
	}
	return t, nil
}
