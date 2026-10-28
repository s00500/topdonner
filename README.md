# topdonner

Get real temperature data out of a **Topdon TC004** thermal camera on a Mac —
live over USB, or from the `.irg` files it saves.

## The camera

The TC004 is an InfiRay-based handheld. Over USB it shows up as
"Iray MCamera" (`12d1:0000`) and has two modes:

- **USB storage** — exposes `DCIM/` with a `.jpg` and a radiometric `.irg` per shot.
  The `.irg` holds the full 180×240 temperature map (Kelvin ×10, uint16 LE)
  plus emissivity, distance, reflected and air temperature. The same format is
  used by other InfiRay cameras (C200, P200), which `topdonner` also reads.
- **Webcam (UVC)** — streams two interleaved frame kinds on one endpoint:
  - *thermal*: a 10-byte `FF 00 FF 00 …` marker followed by the raw 180×240
    Kelvin ×10 values, ~4.5 new frames/s
  - *video*: the camera's own rendered picture (palette applied, stretched to
    16:9) as MJPEG at ~24 fps

## The macOS problem

Plug the TC004 in as a webcam and macOS sees it, but no app ever gets a frame:
`UVCAssistant` never manages to stream from it. On top of that, a normal
webcam app would only ever show the colourised video, never the temperatures.

Two things were needed to get data out:

1. **Bypass UVCAssistant.** `topdonner` talks to the device directly through
   libusb, detaching the macOS driver while it runs (hence `sudo`) and handing
   the camera back on exit.
2. **Negotiate like Windows/Linux do.** The camera only streams properly if the
   probe control is *queried* (`GET_LEN/INFO/DEF/MIN/MAX/CUR`) before the UVC
   probe/commit handshake. Skip that and it sends nothing, or only 1280×720 YUV
   instead of MJPEG.

The thermal frames are then picked out of the stream by their marker and size.

## Why this exists

The camera's own app and screen give you a colour picture and a few spot
readings. We wanted the actual per-pixel temperatures — to log, analyse and
render ourselves — both live on the Mac and from shots taken in the field,
without Windows or the vendor software.

## Usage

```sh
brew install libusb pkg-config
go build

./topdonner info FILE.irg...           # parameters and min/max/mean/centre
./topdonner convert [-o DIR] PATH...   # NAME.png (ironbow) + NAME.csv (°C)
./topdonner import [-o captures]       # copy new shots off the camera (storage mode) and convert
sudo ./topdonner live                  # live view in the browser (webcam mode)
```

The live view serves the raw frames at `/frame`, the camera's video at
`/video.mjpg`, and saves snapshots (`.png`, `.csv`, `.raw`) to `captures/`.

Format references: [heatseeker](https://github.com/crroush/heatseeker),
[irgdump](https://git.sr.ht/~phlash/irgdump),
[infiray_irg](https://github.com/jaseg/infiray_irg).
