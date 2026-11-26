// Command genicons writes the extension's PNG icons so the build stays
// dependency-free (no image library, no binary assets in the tree).
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

func main() {
	out := "extension/icons"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, size := range []int{16, 32, 48, 128} {
		if err := write(filepath.Join(out, fmt.Sprintf("logo%d.png", size)), size); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fmt.Println("wrote icons to", out)
}

func write(path string, size int) error {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{0x1f, 0x6f, 0xeb, 0xff}      // brand blue
	fg := color.RGBA{0xff, 0xff, 0xff, 0xff}      // arrow
	hi := color.RGBA{0x4d, 0xab, 0xf7, 0xff}      // top highlight

	radius := float64(size) * 0.22
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if insideRoundRect(x, y, size, radius) {
				img.Set(x, y, blend(bg, hi, float64(y)/float64(size)*0.45))
			}
		}
	}
	// Down arrow: shaft plus head, scaled to the icon.
	cx := float64(size) / 2
	shaftW := float64(size) * 0.16
	shaftTop := float64(size) * 0.18
	shaftBot := float64(size) * 0.52
	for y := int(shaftTop); y < int(shaftBot); y++ {
		for x := int(cx - shaftW/2); x <= int(cx+shaftW/2); x++ {
			if inBounds(x, y, size) {
				img.Set(x, y, fg)
			}
		}
	}
	// Arrow head.
	headTop := float64(size) * 0.46
	headBot := float64(size) * 0.72
	half := float64(size) * 0.26
	steps := int(headBot - headTop)
	for i := 0; i <= steps; i++ {
		y := int(headTop) + i
		w := half * (1 - float64(i)/float64(steps+1))
		for x := int(cx - w); x <= int(cx+w); x++ {
			if inBounds(x, y, size) {
				img.Set(x, y, fg)
			}
		}
	}
	// Baseline tray under the arrow.
	trayY := int(float64(size) * 0.80)
	trayH := int(float64(size) * 0.075)
	if trayH < 1 {
		trayH = 1
	}
	for y := trayY; y < trayY+trayH; y++ {
		for x := int(float64(size) * 0.26); x < int(float64(size)*0.74); x++ {
			if inBounds(x, y, size) {
				img.Set(x, y, fg)
			}
		}
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func inBounds(x, y, size int) bool { return x >= 0 && y >= 0 && x < size && y < size }

func insideRoundRect(x, y, size int, r float64) bool {
	fx, fy := float64(x)+0.5, float64(y)+0.5
	s := float64(size)
	if fx < r && fy < r {
		return dist(fx, fy, r, r) <= r
	}
	if fx > s-r && fy < r {
		return dist(fx, fy, s-r, r) <= r
	}
	if fx < r && fy > s-r {
		return dist(fx, fy, r, s-r) <= r
	}
	if fx > s-r && fy > s-r {
		return dist(fx, fy, s-r, s-r) <= r
	}
	return true
}

func dist(x1, y1, x2, y2 float64) float64 {
	dx, dy := x1-x2, y1-y2
	return sqrt(dx*dx + dy*dy)
}

func sqrt(v float64) float64 {
	if v <= 0 {
		return 0
	}
	x := v
	for i := 0; i < 24; i++ {
		x = 0.5 * (x + v/x)
	}
	return x
}

func blend(a, b color.RGBA, t float64) color.RGBA {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	mix := func(x, y uint8) uint8 { return uint8(float64(x)*(1-t) + float64(y)*t) }
	return color.RGBA{mix(a.R, b.R), mix(a.G, b.G), mix(a.B, b.B), 0xff}
}
