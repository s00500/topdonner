package uvc

/*
#cgo pkg-config: libusb-1.0
#include <libusb.h>
#include <stdint.h>
#include <stdlib.h>

extern void goTransferDone(struct libusb_transfer *t);

static void transfer_cb(struct libusb_transfer *t) { goTransferDone(t); }

static struct libusb_transfer *alloc_iso(libusb_device_handle *h, unsigned char ep, int npackets, int packetSize) {
	struct libusb_transfer *t = libusb_alloc_transfer(npackets);
	if (!t) return NULL;
	unsigned char *buf = malloc((size_t)npackets * packetSize);
	libusb_fill_iso_transfer(t, h, ep, buf, npackets * packetSize, npackets, transfer_cb, NULL, 1000);
	libusb_set_iso_packet_lengths(t, packetSize);
	return t;
}

static void free_iso(struct libusb_transfer *t) {
	free(t->buffer);
	libusb_free_transfer(t);
}

static unsigned char *packet_buf(struct libusb_transfer *t, int i) { return libusb_get_iso_packet_buffer_simple(t, i); }
static int packet_len(struct libusb_transfer *t, int i) { return t->iso_packet_desc[i].actual_length; }
static int packet_status(struct libusb_transfer *t, int i) { return t->iso_packet_desc[i].status; }
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

const (
	ifControl   = 0
	ifStreaming = 1
	streamEP    = 0x81
	streamAlt   = 1
	packetSize  = 3 * 1024 // high-bandwidth iso: 3 transactions of 1024 per microframe
	numPackets  = 32
	numXfers    = 8

	setCur  = 0x01
	getCur  = 0x81
	getMin  = 0x82
	getMax  = 0x83
	getLen  = 0x85
	getInfo = 0x86
	getDef  = 0x87

	vsProbe  = 0x01
	vsCommit = 0x02
)

// Format/frame indices from the camera's descriptors (see usb-info).
type Mode struct {
	Format, Frame byte
	Name          string
}

var Modes = map[string]Mode{
	"yuy2-480":   {1, 1, "YUY2 640x480"},
	"mjpeg-360":  {2, 1, "MJPEG 640x360"},
	"mjpeg-720":  {2, 2, "MJPEG 1280x720"},
	"mjpeg-1080": {2, 3, "MJPEG 1920x1080"},
	"h264-360":   {3, 1, "H264 640x360"},
}

// Stream owns the camera between Open and Close. While started, every
// non-empty iso packet is handed to OnPacket on the libusb event goroutine;
// the slice is only valid during the call.
type Stream struct {
	ctx      *C.libusb_context
	h        *C.libusb_device_handle
	xfers    []*C.struct_libusb_transfer
	pending  atomic.Int32
	stopping atomic.Bool
	done     chan struct{}

	OnPacket func(data []byte)
	Probe    []byte // the committed probe/commit block

	Packets, Empty, Errors atomic.Int64
}

var (
	activeMu sync.Mutex
	active   *Stream
)

// Open takes the camera from macOS's UVC driver; claiming needs root.
func Open() (*Stream, error) {
	s := &Stream{}
	if err := check(C.libusb_init(&s.ctx)); err != nil {
		return nil, err
	}
	dev, err := findDevice(s.ctx)
	if err != nil {
		C.libusb_exit(s.ctx)
		return nil, err
	}
	rc := C.libusb_open(dev, &s.h)
	C.libusb_unref_device(dev)
	if rc < 0 {
		C.libusb_exit(s.ctx)
		return nil, fmt.Errorf("open: %w", usbError(rc))
	}
	C.libusb_set_auto_detach_kernel_driver(s.h, 1)
	for _, itf := range []C.int{ifControl, ifStreaming} {
		if rc := C.libusb_claim_interface(s.h, itf); rc < 0 {
			s.release()
			if rc == C.LIBUSB_ERROR_ACCESS {
				return nil, fmt.Errorf("claim interface %d: %w (run with sudo so macOS's UVCAssistant can be detached)", itf, usbError(rc))
			}
			return nil, fmt.Errorf("claim interface %d: %w", itf, usbError(rc))
		}
	}
	activeMu.Lock()
	active = s
	activeMu.Unlock()
	return s, nil
}

// ProbeInfo returns the camera's answers to the probe control queries
// (GET_LEN, GET_INFO, GET_DEF, GET_MIN, GET_MAX, GET_CUR) for diagnostics.
func (s *Stream) ProbeInfo() map[string][]byte {
	out := map[string][]byte{}
	for _, q := range []struct {
		name string
		req  C.uint8_t
		n    int
	}{{"LEN", getLen, 2}, {"INFO", getInfo, 1}, {"DEF", getDef, 34}, {"MIN", getMin, 34}, {"MAX", getMax, 34}, {"CUR", getCur, 34}} {
		b := make([]byte, q.n)
		n, err := s.control(q.req, vsProbe, b)
		if err != nil {
			out[q.name] = []byte(err.Error())
			continue
		}
		out[q.name] = b[:n]
	}
	return out
}

// Start negotiates mode, selects the streaming alt setting and keeps iso
// transfers in flight until Stop.
func (s *Stream) Start(mode Mode, onPacket func([]byte)) error {
	s.OnPacket = onPacket
	s.Packets.Store(0)
	s.Empty.Store(0)
	s.Errors.Store(0)
	s.stopping.Store(false)
	s.done = make(chan struct{})
	if err := s.negotiate(mode); err != nil {
		return err
	}
	if err := check(C.libusb_set_interface_alt_setting(s.h, ifStreaming, streamAlt)); err != nil {
		return fmt.Errorf("select alt setting: %w", err)
	}
	for range numXfers {
		t := C.alloc_iso(s.h, streamEP, numPackets, packetSize)
		if t == nil {
			s.Stop()
			return fmt.Errorf("allocating transfer")
		}
		s.xfers = append(s.xfers, t)
		if err := check(C.libusb_submit_transfer(t)); err != nil {
			s.Stop()
			return fmt.Errorf("submit: %w", err)
		}
		s.pending.Add(1)
	}
	go s.loop()
	return nil
}

// negotiate runs the UVC probe/commit handshake for the requested mode.
func (s *Stream) negotiate(mode Mode) error {
	// Windows and Linux query the probe control before setting it. The only
	// runs that got frames did the same; the ones that skipped it got none.
	s.ProbeInfo()

	probe := make([]byte, 34)                   // UVC 1.1 probe/commit block
	binary.LittleEndian.PutUint16(probe[0:], 1) // bmHint: keep dwFrameInterval
	probe[2], probe[3] = mode.Format, mode.Frame
	binary.LittleEndian.PutUint32(probe[4:], 333333) // 30 fps

	if _, err := s.control(setCur, vsProbe, probe); err != nil {
		return fmt.Errorf("probe set: %w", err)
	}
	if _, err := s.control(getCur, vsProbe, probe); err != nil {
		return fmt.Errorf("probe get: %w", err)
	}
	if _, err := s.control(setCur, vsCommit, probe); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	s.Probe = probe
	return nil
}

func (s *Stream) control(req C.uint8_t, selector int, data []byte) (int, error) {
	reqType := C.uint8_t(0x21) // class, interface, host-to-device
	if req&0x80 != 0 {
		reqType = 0xA1
	}
	rc := C.libusb_control_transfer(s.h, reqType, req, C.uint16_t(selector<<8), ifStreaming,
		(*C.uchar)(unsafe.Pointer(&data[0])), C.uint16_t(len(data)), 1000)
	return int(rc), check(rc)
}

func (s *Stream) loop() {
	runtime.LockOSThread()
	tv := C.struct_timeval{tv_usec: 100_000}
	for s.pending.Load() > 0 {
		C.libusb_handle_events_timeout(s.ctx, &tv)
	}
	close(s.done)
}

//export goTransferDone
func goTransferDone(t *C.struct_libusb_transfer) {
	activeMu.Lock()
	s := active
	activeMu.Unlock()
	if t.status == C.LIBUSB_TRANSFER_COMPLETED {
		for i := range C.int(t.num_iso_packets) {
			n := C.packet_len(t, i)
			switch {
			case C.packet_status(t, i) != 0:
				s.Errors.Add(1)
			case n == 0:
				s.Empty.Add(1)
			default:
				s.Packets.Add(1)
				if s.OnPacket != nil {
					s.OnPacket(unsafe.Slice((*byte)(unsafe.Pointer(C.packet_buf(t, i))), n))
				}
			}
		}
	} else if t.status != C.LIBUSB_TRANSFER_CANCELLED {
		s.Errors.Add(1)
	}
	if s.stopping.Load() || t.status == C.LIBUSB_TRANSFER_NO_DEVICE || C.libusb_submit_transfer(t) < 0 {
		s.pending.Add(-1)
	}
}

// Done is closed when streaming ends, by Stop or because the camera went away.
func (s *Stream) Done() <-chan struct{} { return s.done }

// Stop cancels the transfers and selects the zero-bandwidth alt setting,
// which tells the camera to stop streaming.
func (s *Stream) Stop() {
	if s.stopping.Swap(true) || len(s.xfers) == 0 {
		return
	}
	for _, t := range s.xfers {
		C.libusb_cancel_transfer(t)
	}
	select {
	case <-s.done:
		for _, t := range s.xfers {
			C.free_iso(t)
		}
	case <-time.After(3 * time.Second): // leak rather than free transfers still in flight
	}
	s.xfers = nil
	C.libusb_set_interface_alt_setting(s.h, ifStreaming, 0)
}

// Close stops streaming and hands the camera back to macOS.
func (s *Stream) Close() {
	s.Stop()
	s.release()
}

func (s *Stream) release() {
	C.libusb_release_interface(s.h, ifStreaming)
	C.libusb_release_interface(s.h, ifControl) // auto-detach reattaches UVCAssistant
	C.libusb_close(s.h)
	C.libusb_exit(s.ctx)
}
