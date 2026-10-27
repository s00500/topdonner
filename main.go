// topdonner reads radiometric .irg files from a Topdon TC004 (and other
// InfiRay-based cameras) and turns them into temperature data.
package main

import (
	"flag"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"

	"topdonner/uvc"
)

const usage = `usage:
  topdonner info FILE.irg...                 print parameters and min/max/mean/centre
  topdonner convert [-o DIR] [-scale N] PATH...
                                             write NAME.png (ironbow) and NAME.csv (°C)
                                             for each .irg file or directory of them
  topdonner import [-o DIR] [-scale N] [-src DIR]
                                             copy new .irg/.jpg files off the camera
                                             (mounted in USB storage mode) and convert them
  topdonner live [-addr HOST:PORT] [-o DIR]  live raw temperatures from the camera in webcam mode,
                                             shown in the browser (needs sudo after replugging)
  topdonner usb-info                         dump the camera's USB/UVC descriptors (webcam mode)
  sudo topdonner usb-record [-mode all|M,...] [-s SECS] [-o DIR]
                                             take the camera from macOS and record raw packets
  topdonner usb-analyze [-jpegs DIR] FILE    inspect a recording, extract JPEG frames`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "info":
		err = cmdInfo(args)
	case "convert":
		err = cmdConvert(args)
	case "import":
		err = cmdImport(args)
	case "live":
		err = cmdLive(args)
	case "usb-info":
		err = uvc.DumpDescriptors(os.Stdout)
	case "usb-record":
		err = cmdUSBRecord(args)
	case "usb-analyze":
		err = cmdUSBAnalyze(args)
	case "usb-frames":
		err = cmdUSBFrames(args)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "topdonner:", err)
		os.Exit(1)
	}
}

func cmdInfo(args []string) error {
	for _, p := range args {
		t, err := ReadIRG(p)
		if err != nil {
			return err
		}
		fmt.Printf("%s\n  %s  %dx%d  emissivity %.2f  distance %.1fm",
			p, t.Model, t.W, t.H, t.Emissivity, t.Distance)
		if t.Model == "Topdon TC004" {
			fmt.Printf("  reflected %.1f°C  air %.1f°C", t.Reflected, t.Air)
		}
		fmt.Printf("\n  %s\n", t.Stats())
	}
	return nil
}

func cmdConvert(args []string) error {
	fs := flag.NewFlagSet("convert", flag.ExitOnError)
	out := fs.String("o", "", "output directory (default: next to each input)")
	scale := fs.Int("scale", 3, "PNG upscale factor")
	fs.Parse(args)

	files, err := irgFiles(fs.Args())
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no .irg files given")
	}
	for _, f := range files {
		if err := convert(f, *out, *scale); err != nil {
			return err
		}
	}
	return nil
}

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	out := fs.String("o", "captures", "where to copy and convert the files")
	scale := fs.Int("scale", 3, "PNG upscale factor")
	src := fs.String("src", "", "camera DCIM directory (default: search /Volumes)")
	fs.Parse(args)

	if *src == "" {
		found, err := findCameraVolume()
		if err != nil {
			return err
		}
		*src = found
	}
	files, err := irgFiles([]string{*src})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	var n int
	for _, f := range files {
		dst := filepath.Join(*out, filepath.Base(f))
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := copyFile(f, dst); err != nil {
			return err
		}
		// The camera's own JPG of the same shot, with its overlay.
		jpg := strings.TrimSuffix(f, filepath.Ext(f)) + ".jpg"
		if _, err := os.Stat(jpg); err == nil {
			copyFile(jpg, strings.TrimSuffix(dst, filepath.Ext(dst))+".jpg")
		}
		if err := convert(dst, "", *scale); err != nil {
			return err
		}
		n++
	}
	fmt.Printf("imported %d new of %d files from %s into %s\n", n, len(files), *src, *out)
	return nil
}

func convert(path, outDir string, scale int) error {
	t, err := ReadIRG(path)
	if err != nil {
		return err
	}
	if outDir == "" {
		outDir = filepath.Dir(path)
	} else if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	base := filepath.Join(outDir, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	if err := writeFile(base+".png", func(w io.Writer) error { return png.Encode(w, t.Image(scale)) }); err != nil {
		return err
	}
	if err := writeFile(base+".csv", t.WriteCSV); err != nil {
		return err
	}
	fmt.Printf("%s: %s\n", filepath.Base(path), t.Stats())
	return nil
}

// irgFiles expands directories (recursively) into the .irg files they contain.
func irgFiles(paths []string) ([]string, error) {
	var files []string
	for _, p := range paths {
		err := filepath.WalkDir(p, func(f string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.EqualFold(filepath.Ext(f), ".irg") && !strings.HasPrefix(d.Name(), "._") {
				files = append(files, f)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

// findCameraVolume returns the DCIM folder of a mounted volume holding .irg files.
func findCameraVolume() (string, error) {
	dirs, _ := filepath.Glob("/Volumes/*/DCIM")
	for _, d := range dirs {
		if files, _ := irgFiles([]string{d}); len(files) > 0 {
			return d, nil
		}
	}
	return "", fmt.Errorf("no mounted camera with .irg files found under /Volumes/*/DCIM; switch the TC004 to USB storage mode or pass -src")
}

func writeFile(path string, write func(io.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := write(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeFile(dst, func(w io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	})
}
