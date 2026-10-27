package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"topdonner/uvc"
)

// Order matters for -mode all: YUY2 first, since that is what Linux defaults
// to and the only mode anyone has seen a frame in.
var sweepOrder = []string{"yuy2-480", "mjpeg-360", "mjpeg-720", "mjpeg-1080", "h264-360"}

func cmdUSBRecord(args []string) error {
	fs := flag.NewFlagSet("usb-record", flag.ExitOnError)
	modeList := fs.String("mode", "all", "comma-separated stream modes, or all: "+strings.Join(sweepOrder, ", "))
	secs := fs.Float64("s", 4, "seconds to record per mode")
	out := fs.String("o", ".scratchpad", "output directory for stream_<mode>.bin")
	fs.Parse(args)

	names := strings.Split(*modeList, ",")
	if *modeList == "all" {
		names = sweepOrder
	}
	for _, n := range names {
		if _, ok := uvc.Modes[n]; !ok {
			return fmt.Errorf("unknown mode %q", n)
		}
	}

	s, err := uvc.Open()
	if err != nil {
		return err
	}
	defer s.Close()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	info := s.ProbeInfo()
	for _, k := range []string{"LEN", "INFO", "DEF", "MIN", "MAX", "CUR"} {
		fmt.Printf("probe %-4s % x\n", k, info[k])
		if len(info[k]) >= 26 {
			fmt.Printf("           %s\n", probeString(info[k]))
		}
	}

	for _, n := range names {
		if err := recordMode(s, uvc.Modes[n], *secs, filepath.Join(*out, "stream_"+n+".bin"), stop); err != nil {
			return err
		}
	}
	return nil
}

func recordMode(s *uvc.Stream, mode uvc.Mode, secs float64, path string, stop chan os.Signal) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	defer w.Flush()

	var mu sync.Mutex
	var bytes int64
	err = s.Start(mode, func(p []byte) {
		mu.Lock()
		uvc.WritePacket(w, p)
		bytes += int64(len(p))
		mu.Unlock()
	})
	if err != nil {
		return fmt.Errorf("%s: %w", mode.Name, err)
	}
	defer s.Stop()
	fmt.Printf("%s: committed %s\n", mode.Name, probeString(s.Probe))

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	deadline := time.After(time.Duration(secs * float64(time.Second)))
	for {
		select {
		case <-tick.C:
			mu.Lock()
			fmt.Printf("  packets %d  empty %d  errors %d  bytes %d\n", s.Packets.Load(), s.Empty.Load(), s.Errors.Load(), bytes)
			mu.Unlock()
		case <-deadline:
			fmt.Printf("  wrote %s\n", path)
			return nil
		case <-stop:
			return fmt.Errorf("interrupted")
		}
	}
}

func probeString(p []byte) string {
	le := binary.LittleEndian
	return fmt.Sprintf("hint %#x format %d frame %d interval %d maxframe %d maxpayload %d",
		le.Uint16(p), p[2], p[3], le.Uint32(p[4:]), le.Uint32(p[18:]), le.Uint32(p[22:]))
}

func cmdUSBAnalyze(args []string) error {
	fs := flag.NewFlagSet("usb-analyze", flag.ExitOnError)
	jpegDir := fs.String("jpegs", "", "write extracted JPEG frames here")
	n := fs.Int("n", 10, "max number of JPEG frames to write")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: usb-analyze [-jpegs DIR] FILE")
	}
	return uvc.Analyze(fs.Arg(0), os.Stdout, *jpegDir, *n)
}

func modeNames() []string {
	var names []string
	for k := range uvc.Modes {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// cmdUSBFrames renders the first thermal and video frame of a recording, in
// both candidate orientations, to work out the layout.
func cmdUSBFrames(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: usb-frames RECORDING OUTDIR")
	}
	if err := os.MkdirAll(args[1], 0o755); err != nil {
		return err
	}
	var gotThermal, gotVideo bool
	sizes := map[int]int{}
	err := uvc.Replay(args[0], func(f []byte) {
		sizes[len(f)]++
		if k, ok := uvc.ThermalFrame(f); ok && !gotThermal {
			gotThermal = true
			for _, wh := range [][2]int{{uvc.ThermalW, uvc.ThermalH}, {uvc.ThermalH, uvc.ThermalW}} {
				t := &Thermal{W: wh[0], H: wh[1], C: make([]float64, len(k))}
				for i, v := range k {
					t.C[i] = float64(v)/10 - 273.15
				}
				name := filepath.Join(args[1], fmt.Sprintf("thermal_%dx%d.png", wh[0], wh[1]))
				writeFile(name, func(w io.Writer) error { return png.Encode(w, t.Image(2)) })
				fmt.Printf("%s: %s\n", name, t.Stats())
			}
		} else if !ok && len(f) == 1382400 && !gotVideo {
			gotVideo = true
			for _, wh := range [][2]int{{1280, 720}, {720, 1280}, {960, 960}} {
				img := image.NewGray(image.Rect(0, 0, wh[0], wh[1]))
				copy(img.Pix, f[:wh[0]*wh[1]])
				name := filepath.Join(args[1], fmt.Sprintf("video_y_%dx%d.png", wh[0], wh[1]))
				writeFile(name, func(w io.Writer) error { return png.Encode(w, img) })
			}
		}
	})
	fmt.Println("frame sizes:", sizes)
	return err
}

// cmdUSBXU reads (never writes) the vendor extension unit and the standard
// processing unit controls.
func cmdUSBXU(args []string) error {
	s, err := uvc.Open()
	if err != nil {
		return err
	}
	defer s.Close()
	fmt.Println("vendor extension unit:")
	for sel := 1; sel <= 15; sel++ {
		s.DumpUnitControl(os.Stdout, uvc.XUUnit, sel)
	}
	fmt.Println("processing unit (2):")
	for sel := 1; sel <= 19; sel++ {
		s.DumpUnitControl(os.Stdout, 2, sel)
	}
	return nil
}

// cmdUSBXUTest writes each value in the advertised range (0..6) to the vendor
// control while streaming, measures what changes, and restores the original.
func cmdUSBXUTest(args []string) error {
	fs := flag.NewFlagSet("usb-xu-test", flag.ExitOnError)
	sel := fs.Int("sel", uvc.XUSelector, "extension unit selector")
	secs := fs.Float64("s", 4, "seconds to measure per value")
	out := fs.String("o", ".scratchpad/xutest", "where to save sample frames")
	fs.Parse(args)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	chownToSudoUser(*out)

	s, err := uvc.Open()
	if err != nil {
		return err
	}
	defer s.Close()
	orig, err := s.GetXU(*sel)
	if err != nil {
		return fmt.Errorf("reading original value: %w", err)
	}
	restore := append(append([]byte(nil), orig...), make([]byte, 4)...)[:4]
	defer func() {
		err := s.SetXU(*sel, restore)
		back, _ := s.GetXU(*sel)
		fmt.Printf("restored selector %d to % x (error %v, reads back % x)\n", *sel, restore, err, back)
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	// Window measurements, updated from the libusb event goroutine.
	var (
		mu                    sync.Mutex
		uniqT, dupT, nV       int
		sumC                  float64
		sumY, sumU, sumV, nUV float64
		lastT                 []uint16
		sampleT               []uint16
		sampleV               []byte
	)
	reset := func() {
		mu.Lock()
		uniqT, dupT, nV, sumC, sumY, sumU, sumV, nUV = 0, 0, 0, 0, 0, 0, 0, 0
		sampleT, sampleV = nil, nil
		mu.Unlock()
	}
	a := uvc.NewAssembler(func(f []byte) {
		mu.Lock()
		defer mu.Unlock()
		if k, ok := uvc.ThermalFrame(f); ok {
			if slices.Equal(k, lastT) {
				dupT++
				return
			}
			lastT, sampleT = k, k
			uniqT++
			var sum float64
			for _, v := range k {
				sum += float64(v)/10 - 273.15
			}
			sumC += sum / float64(len(k))
			return
		}
		nV++
		const ySize = 1280 * 720
		if len(f) < ySize*3/2 {
			return
		}
		for i := 0; i < ySize; i += 97 {
			sumY += float64(f[i])
		}
		for i := ySize; i+1 < ySize*3/2; i += 98 {
			sumU += float64(f[i])
			sumV += float64(f[i+1])
			nUV++
		}
		sampleV = append(sampleV[:0], f...)
	})
	if err := s.Start(uvc.Modes["mjpeg-360"], a.Push); err != nil {
		return err
	}
	time.Sleep(2 * time.Second) // let the stream get going

	measure := func(label string) error {
		time.Sleep(time.Second)
		reset()
		select {
		case <-time.After(time.Duration(*secs * float64(time.Second))):
		case <-stop:
			return fmt.Errorf("interrupted")
		}
		mu.Lock()
		defer mu.Unlock()
		yN := float64(nV) * float64(1280*720/97+1)
		fmt.Printf("%-10s thermal %.1f fps (+%d repeats)  video %.1f fps  mean %.2f°C  video Y %.1f  chroma %.1f/%.1f\n",
			label, float64(uniqT)/(*secs), dupT, float64(nV)/(*secs), sumC/math.Max(1, float64(uniqT)),
			sumY/math.Max(1, yN), sumU/math.Max(1, nUV), sumV/math.Max(1, nUV))
		if sampleT != nil {
			f := &liveFrame{K10: sampleT}
			name := filepath.Join(*out, "thermal_"+label+".png")
			writeFile(name, func(w io.Writer) error { return png.Encode(w, f.Thermal().Image(2)) })
			chownToSudoUser(name)
		}
		if sampleV != nil {
			img := image.NewGray(image.Rect(0, 0, 1280, 720))
			copy(img.Pix, sampleV)
			name := filepath.Join(*out, "video_"+label+".png")
			writeFile(name, func(w io.Writer) error { return png.Encode(w, img) })
			chownToSudoUser(name)
		}
		return nil
	}

	fmt.Printf("selector %d original value % x\n", *sel, orig)
	if err := measure("baseline"); err != nil {
		return err
	}
	for v := byte(0); v <= 6; v++ {
		werr := s.SetXU(*sel, []byte{v, 0, 0, 0})
		back, rerr := s.GetXU(*sel)
		fmt.Printf("set %d: write error %v, reads back % x (read error %v)\n", v, werr, back, rerr)
		if err := measure(fmt.Sprintf("value_%d", v)); err != nil {
			return err
		}
		select {
		case <-s.Done():
			return fmt.Errorf("camera went away after writing %d", v)
		default:
		}
	}
	return nil
}
