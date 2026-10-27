package uvc

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
)

// UVC payload header bits (bmHeaderInfo).
const (
	hdrFID = 1 << 0
	hdrEOF = 1 << 1
	hdrERR = 1 << 6
)

// Recordings are a sequence of [u16 LE length][payload] records, one per
// non-empty iso packet, exactly as received.
func WritePacket(w io.Writer, p []byte) error {
	var n [2]byte
	binary.LittleEndian.PutUint16(n[:], uint16(len(p)))
	if _, err := w.Write(n[:]); err != nil {
		return err
	}
	_, err := w.Write(p)
	return err
}

func readPackets(r io.Reader, fn func([]byte)) error {
	br := bufio.NewReader(r)
	var n [2]byte
	buf := make([]byte, 65536)
	for {
		if _, err := io.ReadFull(br, n[:]); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		p := buf[:binary.LittleEndian.Uint16(n[:])]
		if _, err := io.ReadFull(br, p); err != nil {
			return err
		}
		fn(p)
	}
}

// Analyze reports how the recorded payload headers behave and extracts any
// JPEG frames found by scanning the payload bodies for SOI/EOI markers.
func Analyze(path string, w io.Writer, jpegDir string, maxJPEGs int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var (
		packets, fidToggles, eofs, errs, bodyBytes int
		hdrLens                                    = map[int]int{}
		infos                                      = map[byte]int{}
		lastFID                                    = -1
		fidFrameSizes                              []int
		curFrame                                   int
		firstHeaders                               [][]byte
		body                                       bytes.Buffer
	)
	err = readPackets(f, func(p []byte) {
		packets++
		if len(firstHeaders) < 12 {
			firstHeaders = append(firstHeaders, append([]byte(nil), p[:min(len(p), 16)]...))
		}
		hlen := int(p[0])
		hdrLens[hlen]++
		if hlen < 2 || hlen > len(p) {
			return
		}
		info := p[1]
		infos[info]++
		fid := int(info & hdrFID)
		if lastFID >= 0 && fid != lastFID {
			fidToggles++
			fidFrameSizes = append(fidFrameSizes, curFrame)
			curFrame = 0
		}
		lastFID = fid
		if info&hdrEOF != 0 {
			eofs++
		}
		if info&hdrERR != 0 {
			errs++
		}
		curFrame += len(p) - hlen
		bodyBytes += len(p) - hlen
		body.Write(p[hlen:])
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "packets %d, payload bytes %d\n", packets, bodyBytes)
	fmt.Fprintf(w, "header lengths: %v\n", hdrLens)
	fmt.Fprintf(w, "header info bytes:")
	for _, k := range sortedKeys(infos) {
		fmt.Fprintf(w, " %#02x×%d", k, infos[k])
	}
	fmt.Fprintf(w, "\nFID toggles %d, EOF flags %d, ERR flags %d\n", fidToggles, eofs, errs)
	if len(fidFrameSizes) > 0 {
		fmt.Fprintf(w, "bytes per FID frame (first 10): %v\n", fidFrameSizes[:min(10, len(fidFrameSizes))])
	}
	fmt.Fprintln(w, "first packets:")
	for _, h := range firstHeaders {
		fmt.Fprintf(w, "  % x\n", h)
	}

	jpegs := SplitJPEG(body.Bytes())
	fmt.Fprintf(w, "JPEG frames found by SOI/EOI scan: %d", len(jpegs))
	if len(jpegs) > 0 {
		sizes := make([]int, 0, 10)
		for _, j := range jpegs[:min(10, len(jpegs))] {
			sizes = append(sizes, len(j))
		}
		fmt.Fprintf(w, ", sizes (first 10): %v", sizes)
	}
	fmt.Fprintln(w)
	if jpegDir != "" && len(jpegs) > 0 {
		if err := os.MkdirAll(jpegDir, 0o755); err != nil {
			return err
		}
		step := max(1, len(jpegs)/maxJPEGs)
		for i := 0; i < len(jpegs); i += step {
			name := filepath.Join(jpegDir, fmt.Sprintf("frame_%04d.jpg", i))
			if err := os.WriteFile(name, jpegs[i], 0o644); err != nil {
				return err
			}
		}
		fmt.Fprintf(w, "wrote every %d. frame to %s\n", step, jpegDir)
	}
	return nil
}

// SplitJPEG cuts a byte stream into FF D8 ... FF D9 frames, ignoring any
// framing the transport claims.
func SplitJPEG(b []byte) [][]byte {
	var out [][]byte
	for {
		start := bytes.Index(b, []byte{0xFF, 0xD8, 0xFF})
		if start < 0 {
			return out
		}
		b = b[start:]
		end := bytes.Index(b, []byte{0xFF, 0xD9})
		if end < 0 {
			return out
		}
		// A new SOI before the EOI means the previous frame lost its tail.
		if next := bytes.Index(b[2:end], []byte{0xFF, 0xD8, 0xFF}); next >= 0 {
			b = b[2+next:]
			continue
		}
		out = append(out, b[:end+2])
		b = b[end+2:]
	}
}

func sortedKeys(m map[byte]int) []byte {
	keys := make([]byte, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
