// Package uvc talks to the TC004's USB video gadget directly through libusb,
// bypassing macOS's UVCAssistant, which never gets a frame out of it.
package uvc

/*
#cgo pkg-config: libusb-1.0
#include <libusb.h>
*/
import "C"

import (
	"fmt"
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
