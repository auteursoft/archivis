package main

import (
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/auteursoft/archivis/internal/imageio"
)

func runInspect(g *globals, args []string) error {
	fs := newFlagSet("inspect", `Show what Archivis reads from photo files, without cataloguing them:
the image it decodes (for RAW, the embedded preview) against the recorded size,
orientation, camera, lens, date and exposure, with warnings.
  inspect FILE_OR_FOLDER...
  inspect --previews DIR FOLDER    also save each decoded image, to check rotation by eye`)
	previews := fs.String("previews", "", "save each decoded image (long edge 800 px) as a JPEG in this folder")
	g.register(fs)
	paths := parseInterleaved(fs, args)
	if len(paths) == 0 {
		return fmt.Errorf("usage: archivis inspect [--previews DIR] FILE_OR_FOLDER...")
	}
	return inspect(os.Stdout, paths, *previews)
}

// inspectFile is a file to inspect and the name it is shown and saved
// under: its path relative to the folder it was found in, so that
// DSC00001.ARW in two card folders stays two rows and two previews.
type inspectFile struct{ path, name string }

func inspect(out io.Writer, paths []string, previews string) error {
	if previews != "" {
		if err := os.MkdirAll(previews, 0o755); err != nil {
			return err
		}
	}
	var files []inspectFile
	seen := map[string]int{}
	for _, root := range paths {
		err := filepath.WalkDir(root, func(path string, d iofs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !imageio.IsImagePath(path) {
				return nil
			}
			name := filepath.Base(path)
			if rel, err := filepath.Rel(root, path); err == nil && rel != "." {
				name = filepath.ToSlash(rel)
			}
			if seen[name]++; seen[name] > 1 { // the same name under two arguments
				name = fmt.Sprintf("%s (%d)", name, seen[name])
			}
			files = append(files, inspectFile{path, name})
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(files) == 0 {
		return fmt.Errorf("no photo files found")
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FILE\tFORMAT\tDECODED\tRECORDED\tORIENT\tCAMERA\tLENS\tTAKEN\tEXPOSURE\tTIME\tWARNINGS")
	problems := 0
	for _, f := range files {
		start := time.Now()
		d, err := imageio.Load(f.path, 800)
		took := time.Since(start).Round(time.Millisecond)
		if err != nil {
			problems++
			fmt.Fprintf(tw, "%s\t-\t-\t-\t-\t-\t-\t-\t-\t%s\tERROR: %v\n", f.name, took, err)
			continue
		}
		m := d.Meta
		warn := inspectWarnings(d)
		if len(warn) > 0 {
			problems++
		}
		recorded := "-"
		if m.Width > 0 && m.Height > 0 {
			recorded = fmt.Sprintf("%dx%d", m.Width, m.Height)
		}
		taken := "-"
		if !m.TakenAt.IsZero() {
			taken = m.TakenAt.Format("2006-01-02 15:04:05")
		}
		exp := "-"
		if m.FNumber > 0 || m.ExposureTime > 0 {
			exp = fmt.Sprintf("%.0fmm f/%.1f %s ISO %d", m.FocalLength, m.FNumber, shutterText(m.ExposureTime), m.ISO)
		}
		fmt.Fprintf(tw, "%s\t%s\t%dx%d\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			f.name, d.Format, d.SrcW, d.SrcH, recorded, m.Orientation,
			orDash(strings.TrimSpace(m.Make+" "+m.Model)), orDash(m.Lens), taken, exp, took, strings.Join(warn, "; "))
		if previews != "" {
			b, err := imageio.EncodeJPEG(d.Img, 85)
			if err == nil {
				err = os.WriteFile(filepath.Join(previews, previewName(f.name)), b, 0o644)
			}
			if err != nil {
				return err
			}
		}
	}
	tw.Flush()
	fmt.Fprintf(out, "\n%d files, %d with errors or warnings\n", len(files), problems)
	if previews != "" {
		fmt.Fprintf(out, "decoded images saved in %s: check that each one is upright\n", previews)
	}
	return nil
}

// previewName flattens a shown name into one file name:
// "100MSDCF/DSC00001.ARW" -> "100MSDCF__DSC00001.ARW.jpg".
func previewName(name string) string {
	return strings.NewReplacer("/", "__", " ", "_", "(", "", ")", "").Replace(name) + ".jpg"
}

// inspectWarnings flags what would make a photo catalogue badly.
func inspectWarnings(d *imageio.Decoded) []string {
	var w []string
	m := d.Meta
	if m.Make == "" && m.Model == "" {
		w = append(w, "no camera")
	}
	if m.TakenAt.IsZero() {
		w = append(w, "no capture time")
	}
	// A camera photo should name its lens; scans and screenshots record no
	// camera either, and already say so.
	if m.Lens == "" && (m.Make != "" || m.Model != "" || strings.HasPrefix(d.Format, "raw")) {
		w = append(w, "no lens")
	}
	if strings.HasPrefix(d.Format, "raw") {
		long := max(d.SrcW, d.SrcH)
		if long < 1600 {
			w = append(w, fmt.Sprintf("small preview (%d px long edge)", long))
		}
		// The preview should have the recorded image's shape (either way
		// round). Some formats record only a thumbnail's size; skip those.
		if m.Width > 0 && m.Height > 0 && d.SrcW > 0 && d.SrcH > 0 && max(m.Width, m.Height) >= long {
			sensor := float64(max(m.Width, m.Height)) / float64(min(m.Width, m.Height))
			prev := float64(max(d.SrcW, d.SrcH)) / float64(min(d.SrcW, d.SrcH))
			if prev/sensor > 1.05 || sensor/prev > 1.05 {
				w = append(w, fmt.Sprintf("preview shape %.2f differs from the recorded %.2f", prev, sensor))
			}
		}
	}
	if (m.Orientation == 6 || m.Orientation == 8) && d.SrcW > d.SrcH {
		w = append(w, "rotated 90° but decoded as landscape")
	}
	return w
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func shutterText(s float64) string {
	switch {
	case s <= 0:
		return "-"
	case s < 1:
		return fmt.Sprintf("1/%.0f", 1/s)
	default:
		return fmt.Sprintf("%gs", s)
	}
}
