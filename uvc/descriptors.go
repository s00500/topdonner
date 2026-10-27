// Package uvc talks to the TC004's USB video gadget directly through libusb,
// bypassing macOS's UVCAssistant, which never gets a frame out of it.
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

// The TC004 enumerates as "Iray MCamera" with Huawei's vendor ID.
const (
	VendorID  = 0x12d1
	ProductID = 0x0000
)

type usbError C.int

func (e usbError) Error() string { return C.GoString(C.libusb_error_name(C.int(e))) }

func check(rc C.int) error {
	if rc < 0 {
		return usbError(rc)
	}
	return nil
}

// findDevice returns a referenced libusb_device for the camera.
func findDevice(ctx *C.libusb_context) (*C.libusb_device, error) {
	var list **C.libusb_device
	n := C.libusb_get_device_list(ctx, &list)
	if n < 0 {
		return nil, usbError(n)
	}
	defer C.libusb_free_device_list(list, 1)
	for _, dev := range unsafe.Slice(list, n) {
		var d C.struct_libusb_device_descriptor
		if C.libusb_get_device_descriptor(dev, &d) == 0 && d.idVendor == VendorID && d.idProduct == ProductID {
			return C.libusb_ref_device(dev), nil
		}
	}
	return nil, fmt.Errorf("camera %04x:%04x not found; is the TC004 plugged in and in webcam mode?", VendorID, ProductID)
}

// DumpDescriptors prints the camera's configuration: interfaces, alternate
// settings, endpoints and the class-specific UVC descriptors. Needs no root.
func DumpDescriptors(w io.Writer) error {
	var ctx *C.libusb_context
	if err := check(C.libusb_init(&ctx)); err != nil {
		return err
	}
	defer C.libusb_exit(ctx)
	dev, err := findDevice(ctx)
	if err != nil {
		return err
	}
	defer C.libusb_unref_device(dev)

	var dd C.struct_libusb_device_descriptor
	C.libusb_get_device_descriptor(dev, &dd)
	fmt.Fprintf(w, "device %04x:%04x usb %x.%02x class %d speed %d configs %d\n",
		dd.idVendor, dd.idProduct, dd.bcdUSB>>8, dd.bcdUSB&0xff, dd.bDeviceClass,
		C.libusb_get_device_speed(dev), dd.bNumConfigurations)

	var cfg *C.struct_libusb_config_descriptor
	if err := check(C.libusb_get_active_config_descriptor(dev, &cfg)); err != nil {
		return fmt.Errorf("config descriptor: %w", err)
	}
	defer C.libusb_free_config_descriptor(cfg)
	dumpCS(w, "config", cBytes(cfg.extra, cfg.extra_length), 0)

	for _, itf := range unsafe.Slice(cfg._interface, cfg.bNumInterfaces) {
		for _, alt := range unsafe.Slice(itf.altsetting, itf.num_altsetting) {
			fmt.Fprintf(w, "interface %d alt %d class %d/%d endpoints %d\n",
				alt.bInterfaceNumber, alt.bAlternateSetting, alt.bInterfaceClass, alt.bInterfaceSubClass, alt.bNumEndpoints)
			dumpCS(w, "  ", cBytes(alt.extra, alt.extra_length), alt.bInterfaceSubClass)
			for _, ep := range unsafe.Slice(alt.endpoint, alt.bNumEndpoints) {
				mps := int(ep.wMaxPacketSize)
				fmt.Fprintf(w, "  endpoint %#02x %s maxpacket %d x%d interval %d\n",
					ep.bEndpointAddress, []string{"control", "iso", "bulk", "interrupt"}[ep.bmAttributes&3],
					mps&0x7ff, 1+(mps>>11)&3, ep.bInterval)
				dumpCS(w, "    ", cBytes(ep.extra, ep.extra_length), 0)
			}
		}
	}
	return nil
}

func cBytes(p *C.uchar, n C.int) []byte {
	if p == nil || n <= 0 {
		return nil
	}
	return C.GoBytes(unsafe.Pointer(p), n)
}

var vcSubtypes = map[byte]string{1: "VC_HEADER", 2: "INPUT_TERMINAL", 3: "OUTPUT_TERMINAL", 4: "SELECTOR_UNIT", 5: "PROCESSING_UNIT", 6: "EXTENSION_UNIT"}
var vsSubtypes = map[byte]string{1: "INPUT_HEADER", 3: "STILL_FRAME", 4: "FORMAT_UNCOMPRESSED", 5: "FRAME_UNCOMPRESSED", 6: "FORMAT_MJPEG", 7: "FRAME_MJPEG", 0x0d: "COLORFORMAT", 0x10: "FORMAT_FRAME_BASED", 0x11: "FRAME_FRAME_BASED"}

// dumpCS walks class-specific descriptors (bLength, bDescriptorType 0x24,
// bDescriptorSubtype, ...) and decodes the fields that matter for streaming.
func dumpCS(w io.Writer, indent string, b []byte, subclass C.uint8_t) {
	for len(b) >= 3 && int(b[0]) <= len(b) && b[0] > 0 {
		d := b[:b[0]]
		b = b[b[0]:]
		names := vcSubtypes
		if subclass == 2 {
			names = vsSubtypes
		}
		name := names[d[2]]
		if d[1] != 0x24 || name == "" {
			name = fmt.Sprintf("type %#02x subtype %#02x", d[1], d[2])
		}
		u16 := func(o int) int { return int(binary.LittleEndian.Uint16(d[o:])) }
		u32 := func(o int) int { return int(binary.LittleEndian.Uint32(d[o:])) }
		switch {
		case subclass == 2 && (d[2] == 4 || d[2] == 0x10) && len(d) >= 21:
			fmt.Fprintf(w, "%s%s index %d frames %d fourcc %q bpp %d\n", indent, name, d[3], d[4], d[5:9], d[21])
		case subclass == 2 && d[2] == 6 && len(d) >= 5:
			fmt.Fprintf(w, "%s%s index %d frames %d\n", indent, name, d[3], d[4])
		case subclass == 2 && (d[2] == 5 || d[2] == 7) && len(d) >= 26:
			fmt.Fprintf(w, "%s  %s index %d %dx%d maxbuf %d interval %.1ffps\n", indent, name, d[3], u16(5), u16(7), u32(17), 1e7/float64(u32(21)))
		case subclass == 2 && d[2] == 0x11 && len(d) >= 26:
			fmt.Fprintf(w, "%s  %s index %d %dx%d interval %.1ffps\n", indent, name, d[3], u16(5), u16(7), 1e7/float64(u32(17)))
		default:
			fmt.Fprintf(w, "%s%s % x\n", indent, name, d)
		}
	}
}
