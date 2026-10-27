package main

import (
	"bufio"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
)

// Thermal is one radiometric image: a temperature in °C for every pixel.
type Thermal struct {
	Model      string
	W, H       int
	C          []float64 // row-major
	Emissivity float64
	Distance   float64 // m
	Reflected  float64 // °C, 0 if the file does not carry it
	Air        float64 // °C, 0 if the file does not carry it
	JPEG       []byte  // embedded preview, if any
}

func (t *Thermal) At(x, y int) float64 { return t.C[y*t.W+x] }

type Stats struct {
	Min, Max, Mean, Center float64
	MinX, MinY, MaxX, MaxY int
}

func (t *Thermal) Stats() Stats {
	lo, hi := 0, 0
	var sum float64
	for i, c := range t.C {
		if c < t.C[lo] {
			lo = i
		}
		if c > t.C[hi] {
			hi = i
		}
		sum += c
	}
	return Stats{
		Min: t.C[lo], MinX: lo % t.W, MinY: lo / t.W,
		Max: t.C[hi], MaxX: hi % t.W, MaxY: hi / t.W,
		Mean:   sum / float64(len(t.C)),
		Center: t.At(t.W/2, t.H/2),
	}
}

func (s Stats) String() string {
	return fmt.Sprintf("min %.1f°C (%d,%d)  max %.1f°C (%d,%d)  mean %.1f°C  centre %.1f°C",
		s.Min, s.MinX, s.MinY, s.Max, s.MaxX, s.MaxY, s.Mean, s.Center)
}

// Image renders the temperatures with an ironbow palette stretched over the
// image's own min..max, scaled up by an integer factor.
func (t *Thermal) Image(scale int) image.Image {
	s := t.Stats()
	span := math.Max(s.Max-s.Min, 0.1)
	img := image.NewRGBA(image.Rect(0, 0, t.W*scale, t.H*scale))
	for y := range t.H * scale {
		for x := range t.W * scale {
			img.SetRGBA(x, y, ironbow((t.At(x/scale, y/scale)-s.Min)/span))
		}
	}
	return img
}

// WriteCSV writes one row per image row, temperatures in °C.
func (t *Thermal) WriteCSV(w io.Writer) error {
	bw := bufio.NewWriter(w)
	for y := range t.H {
		for x := range t.W {
			if x > 0 {
				bw.WriteByte(',')
			}
			fmt.Fprintf(bw, "%.2f", t.At(x, y))
		}
		bw.WriteByte('\n')
	}
	return bw.Flush()
}

var ironbowStops = []color.RGBA{
	{0, 0, 0, 255},
	{32, 0, 140, 255},
	{204, 0, 119, 255},
	{255, 165, 0, 255},
	{255, 255, 220, 255},
}

func ironbow(v float64) color.RGBA {
	v = math.Max(0, math.Min(1, v)) * float64(len(ironbowStops)-1)
	i := int(v)
	if i >= len(ironbowStops)-1 {
		return ironbowStops[len(ironbowStops)-1]
	}
	a, b, k := ironbowStops[i], ironbowStops[i+1], v-float64(i)
	mix := func(p, q uint8) uint8 { return uint8(float64(p) + (float64(q)-float64(p))*k) }
	return color.RGBA{mix(a.R, b.R), mix(a.G, b.G), mix(a.B, b.B), 255}
}
