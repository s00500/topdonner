package main

import (
	"encoding/binary"
	"math"
	"testing"
)

// tc004File builds an IRG the way the TC004 writes it: 0x80 header starting
// B0 0B 80 00, 8-bit preview, K x10 temperatures, then a JPEG.
func tc004File(w, h int, kelvin10 func(x, y int) uint16) []byte {
	const hdr = 0x80
	n := w * h
	b := make([]byte, hdr+3*n, hdr+3*n+4)
	copy(b, []byte{0xB0, 0x0B, 0x80, 0x00})
	binary.LittleEndian.PutUint32(b[4:], uint32(n))
	binary.LittleEndian.PutUint16(b[8:], uint16(h))
	binary.LittleEndian.PutUint16(b[10:], uint16(w))
	binary.LittleEndian.PutUint32(b[0x1E:], 9500)      // emissivity 0.95
	binary.LittleEndian.PutUint32(b[0x22:], 2_981_500) // reflected 25 °C
	binary.LittleEndian.PutUint32(b[0x26:], 2_931_500) // air 20 °C
	binary.LittleEndian.PutUint32(b[0x2A:], 10_000)    // 1 m
	b[hdr-2], b[hdr-1] = 0xAC, 0xCA
	for y := range h {
		for x := range w {
			binary.LittleEndian.PutUint16(b[hdr+n+2*(y*w+x):], kelvin10(x, y))
		}
	}
	return append(b, 0xFF, 0xD8, 0xFF, 0xD9)
}

func TestParseTC004(t *testing.T) {
	const w, h = 180, 240
	b := tc004File(w, h, func(x, y int) uint16 {
		if x == 10 && y == 200 {
			return 3731 // 99.95 °C
		}
		return 2931 // 19.95 °C
	})
	th, err := ParseIRG(b)
	if err != nil {
		t.Fatal(err)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 0.01 }
	if th.Model != "Topdon TC004" || th.W != w || th.H != h {
		t.Fatalf("got %s %dx%d", th.Model, th.W, th.H)
	}
	if !near(th.Emissivity, 0.95) || !near(th.Distance, 1) || !near(th.Reflected, 25) || !near(th.Air, 20) {
		t.Errorf("params: %+v", th)
	}
	s := th.Stats()
	if !near(s.Max, 99.95) || s.MaxX != 10 || s.MaxY != 200 || !near(s.Min, 19.95) {
		t.Errorf("stats: %s", s)
	}
	if len(th.JPEG) != 4 {
		t.Errorf("embedded JPEG: %d bytes", len(th.JPEG))
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	b := tc004File(4, 4, func(int, int) uint16 { return 2931 })
	if _, err := ParseIRG(b[:0x90]); err == nil {
		t.Error("accepted truncated file")
	}
	b[0x7E] = 0 // end-of-header marker
	if _, err := ParseIRG(b); err == nil {
		t.Error("accepted file without header end marker")
	}
}
