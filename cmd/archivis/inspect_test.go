package main

import (
	"strings"
	"testing"
	"time"

	"github.com/auteursoft/archivis/internal/imageio"
)

func TestInspectWarnings(t *testing.T) {
	good := imageio.Meta{Make: "SONY", Model: "ILCE-7RM5", Lens: "FE 24-70mm F2.8 GM II", TakenAt: time.Unix(1, 0), Width: 9504, Height: 6336}
	for _, c := range []struct {
		name string
		d    imageio.Decoded
		want []string // substrings expected; nil = no warnings
	}{
		{"clean RAW", imageio.Decoded{Format: "raw:arw", SrcW: 7360, SrcH: 4912, Meta: good}, nil},
		{"small Sony-style preview", imageio.Decoded{Format: "raw:arw", SrcW: 1616, SrcH: 1080, Meta: good}, nil},
		{"tiny preview", imageio.Decoded{Format: "raw:arw", SrcW: 640, SrcH: 424, Meta: good}, []string{"small preview"}},
		{"cropped preview", imageio.Decoded{Format: "raw:nef", SrcW: 1920, SrcH: 1080, Meta: good}, []string{"preview shape"}},
		{"thumbnail-sized record is not compared", imageio.Decoded{Format: "raw:nef", SrcW: 4256, SrcH: 2832,
			Meta: imageio.Meta{Make: "NIKON", Lens: "28mm f/1.4", TakenAt: time.Unix(1, 0), Width: 160, Height: 120}}, nil},
		{"missing metadata", imageio.Decoded{Format: "raw:nef", SrcW: 7360, SrcH: 4912}, []string{"no camera", "no capture time", "no lens"}},
		{"sideways", imageio.Decoded{Format: "heif", SrcW: 4032, SrcH: 3024, Meta: imageio.Meta{Make: "Apple", TakenAt: time.Unix(1, 0), Orientation: 6}}, []string{"decoded as landscape"}},
	} {
		got := strings.Join(inspectWarnings(&c.d), "; ")
		if c.want == nil && got != "" {
			t.Errorf("%s: unexpected warnings %q", c.name, got)
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", c.name, got, w)
			}
		}
	}
}
