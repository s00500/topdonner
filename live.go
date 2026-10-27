package main

import (
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"image/png"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"topdonner/uvc"
)

//go:embed live.html
var liveHTML []byte

// liveFrame is one raw thermal frame as the camera sent it.
type liveFrame struct {
	Seq  int64
	Time time.Time
	K10  []uint16 // Kelvin x10, uvc.ThermalW x uvc.ThermalH
}

func (f *liveFrame) Thermal() *Thermal {
	t := &Thermal{Model: "Topdon TC004 (live)", W: uvc.ThermalW, H: uvc.ThermalH, C: make([]float64, len(f.K10))}
	for i, k := range f.K10 {
		t.C[i] = float64(k)/10 - 273.15
	}
	return t
}

// live holds the latest frame and wakes long-polling HTTP clients.
type live struct {
	mu     sync.Mutex
	frame  *liveFrame
	next   chan struct{} // closed when a newer frame arrives
	status string
	fps    float64
}

func newLive() *live { return &live{next: make(chan struct{}), status: "starting"} }

func (l *live) publish(k10 []uint16) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	seq := int64(1)
	if l.frame != nil {
		seq = l.frame.Seq + 1
		if dt := now.Sub(l.frame.Time).Seconds(); dt > 0 {
			l.fps = 0.8*l.fps + 0.2/dt
		}
	}
	l.frame = &liveFrame{Seq: seq, Time: now, K10: k10}
	l.status = "streaming"
	close(l.next)
	l.next = make(chan struct{})
}

func (l *live) setStatus(s string) {
	l.mu.Lock()
	l.status = s
	l.mu.Unlock()
}

// waitAfter returns the latest frame newer than seq, waiting up to timeout.
func (l *live) waitAfter(ctx context.Context, seq int64, timeout time.Duration) *liveFrame {
	deadline := time.After(timeout)
	for {
		l.mu.Lock()
		f, next := l.frame, l.next
		l.mu.Unlock()
		if f != nil && f.Seq > seq {
			return f
		}
		select {
		case <-next:
		case <-deadline:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

// capture keeps the camera streaming, reconnecting when it drops out or stalls.
func (l *live) capture(ctx context.Context, verbose bool) {
	for ctx.Err() == nil {
		s, err := uvc.Open()
		if err != nil {
			l.setStatus(err.Error())
			if verbose {
				fmt.Fprintln(os.Stderr, "camera:", err)
			}
			sleepCtx(ctx, 2*time.Second)
			continue
		}
		var last time.Time
		var lastMu sync.Mutex
		a := uvc.NewAssembler(func(frame []byte) {
			if k, ok := uvc.ThermalFrame(frame); ok {
				lastMu.Lock()
				last = time.Now()
				lastMu.Unlock()
				l.publish(k)
			}
		})
		// The camera ignores the negotiated format; the smallest one keeps
		// the probe values sane.
		if err := s.Start(uvc.Modes["mjpeg-360"], a.Push); err != nil {
			l.setStatus(err.Error())
			s.Close()
			sleepCtx(ctx, 2*time.Second)
			continue
		}
		l.setStatus("waiting for frames")
		lastMu.Lock()
		last = time.Now()
		lastMu.Unlock()
		tick := time.NewTicker(time.Second)
		reason := "stopped"
	watch:
		for {
			select {
			case <-ctx.Done():
				break watch
			case <-s.Done():
				reason = "camera disconnected"
				break watch
			case <-tick.C:
				lastMu.Lock()
				stalled := time.Since(last) > 5*time.Second
				lastMu.Unlock()
				if stalled {
					reason = "no frames for 5s"
					break watch
				}
			}
		}
		tick.Stop()
		s.Close()
		if ctx.Err() != nil {
			return
		}
		l.setStatus(reason + ", reconnecting")
		if verbose {
			fmt.Fprintf(os.Stderr, "camera: %s, reconnecting\n", reason)
		}
		sleepCtx(ctx, time.Second)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func cmdLive(args []string) error {
	fs := flag.NewFlagSet("live", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8004", "HTTP listen address")
	out := fs.String("o", "captures", "where snapshots are saved")
	scale := fs.Int("scale", 4, "snapshot PNG upscale factor")
	open := fs.Bool("open", true, "open the browser")
	fs.Parse(args)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	l := newLive()
	captureDone := make(chan struct{})
	go func() {
		l.capture(ctx, true)
		close(captureDone)
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(liveHTML)
	})
	// GET /frame?after=SEQ long-polls for the next frame and returns the raw
	// Kelvin x10 values as little-endian uint16, row-major.
	mux.HandleFunc("GET /frame", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		f := l.waitAfter(r.Context(), after, 2*time.Second)
		l.mu.Lock()
		status, fps := l.status, l.fps
		l.mu.Unlock()
		w.Header().Set("X-Status", status)
		w.Header().Set("X-Fps", fmt.Sprintf("%.1f", fps))
		if f == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Seq", strconv.FormatInt(f.Seq, 10))
		w.Header().Set("X-Width", strconv.Itoa(uvc.ThermalW))
		w.Header().Set("X-Height", strconv.Itoa(uvc.ThermalH))
		binary.Write(w, binary.LittleEndian, f.K10)
	})
	mux.HandleFunc("POST /snapshot", func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		f := l.frame
		l.mu.Unlock()
		if f == nil {
			http.Error(w, "no frame yet", http.StatusServiceUnavailable)
			return
		}
		name, err := saveSnapshot(f, *out, *scale)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"name": name, "stats": f.Thermal().Stats().String()})
	})

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()
	url := "http://" + ln.Addr().String()
	fmt.Printf("live view on %s (Ctrl-C to stop)\n", url)
	if *open {
		openBrowser(url)
	}
	err = srv.Serve(ln)
	<-captureDone // hand the camera back to macOS before exiting
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// saveSnapshot writes NAME.png, NAME.csv (°C) and NAME.raw (Kelvin x10, LE
// uint16) for a frame and returns NAME.
func saveSnapshot(f *liveFrame, dir string, scale int) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	base := filepath.Join(dir, "live_"+f.Time.Format("20060102_150405.000"))
	t := f.Thermal()
	if err := writeFile(base+".png", func(w io.Writer) error { return png.Encode(w, t.Image(scale)) }); err != nil {
		return "", err
	}
	if err := writeFile(base+".csv", t.WriteCSV); err != nil {
		return "", err
	}
	if err := writeFile(base+".raw", func(w io.Writer) error { return binary.Write(w, binary.LittleEndian, f.K10) }); err != nil {
		return "", err
	}
	for _, ext := range []string{".png", ".csv", ".raw"} {
		chownToSudoUser(base + ext)
	}
	chownToSudoUser(dir)
	return base, nil
}

// openBrowser opens url in the user's default browser. Under sudo, plain
// "open" would run as root and use root's default browser (Safari).
func openBrowser(url string) {
	if u, uid := os.Getenv("SUDO_USER"), os.Getenv("SUDO_UID"); u != "" && uid != "" {
		exec.Command("launchctl", "asuser", uid, "sudo", "-u", u, "open", url).Start()
		return
	}
	exec.Command("open", url).Start()
}

// chownToSudoUser gives files created under sudo back to the invoking user.
func chownToSudoUser(path string) {
	uid, err1 := strconv.Atoi(os.Getenv("SUDO_UID"))
	gid, err2 := strconv.Atoi(os.Getenv("SUDO_GID"))
	if err1 == nil && err2 == nil {
		os.Chown(path, uid, gid)
	}
}
