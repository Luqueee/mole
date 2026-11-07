//go:build ignore

// Run from desktop/build/tray: go run generate.go
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
)

func main() {
	if err := generate(); err != nil {
		log.Fatal(err)
	}
}

func generate() error {
	f, err := os.Open("mascot.png")
	if err != nil {
		return err
	}
	src, err := png.Decode(f)
	f.Close()
	if err != nil {
		return err
	}
	// Use the complete transparent artwork, not the cropped orange app tile.
	// Discard faint fringe pixels and make the mascot uniformly opaque.
	bounds := src.Bounds()
	mask := image.NewNRGBA(bounds)
	content := image.Rectangle{}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, alpha := src.At(x, y).RGBA()
			if alpha >= 192*257 {
				mask.SetNRGBA(x, y, color.NRGBA{A: 255})
				content = content.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	// Fit the entire figure in a square without stretching or clipping the pick.
	size := max(content.Dx(), content.Dy())
	bounds.Min = image.Pt(content.Min.X-(size-content.Dx())/2, content.Min.Y-(size-content.Dy())/2)
	bounds.Max = bounds.Min.Add(image.Pt(size, size))
	// Area-average to a 22pt Retina image, leaving one point of clear padding.
	out := image.NewNRGBA(image.Rect(0, 0, 44, 44))
	for y := range 40 {
		for x := range 40 {
			var sum, count int
			for sy := y * bounds.Dy() / 40; sy < (y+1)*bounds.Dy()/40; sy++ {
				for sx := x * bounds.Dx() / 40; sx < (x+1)*bounds.Dx()/40; sx++ {
					sum += int(mask.NRGBAAt(bounds.Min.X+sx, bounds.Min.Y+sy).A)
					count++
				}
			}
			out.SetNRGBA(x+2, y+2, color.NRGBA{A: uint8(sum / count)})
		}
	}
	f, err = os.Create("template.png")
	if err != nil {
		return err
	}
	if err := png.Encode(f, out); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
