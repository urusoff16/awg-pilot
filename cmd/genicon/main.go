// genicon renders build/appicon.png (1024×1024): a dark rounded square with a green power symbol.
// Wails derives build/windows/icon.ico from it when the .ico is missing.
//
//	go run ./cmd/genicon build/appicon.png
package main

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

const (
	size = 1024
	ss   = 4 // supersampling per axis
)

var (
	bgTop    = [3]float64{0x24, 0x2e, 0x3b}
	bgBottom = [3]float64{0x10, 0x14, 0x1b}
	green    = [3]float64{0x3f, 0xb6, 0x8b}
)

// shape returns 0 = outside, 1 = background, 2 = symbol at normalized coords (-1..1).
func shape(x, y float64) int {
	// rounded square with corner radius 0.36
	const half, r = 0.94, 0.36
	dx, dy := math.Max(math.Abs(x)-(half-r), 0), math.Max(math.Abs(y)-(half-r), 0)
	if dx*dx+dy*dy > r*r {
		return 0
	}
	// power symbol: ring with a gap at the top plus a vertical bar
	const cy, ringR, w = 0.06, 0.46, 0.13
	d := math.Hypot(x, y-cy)
	angle := math.Atan2(x, -(y - cy)) // 0 = straight up
	if math.Abs(d-ringR) <= w/2 && math.Abs(angle) > 0.62 {
		return 2
	}
	// round caps at the ends of the ring
	for _, s := range []float64{-1, 1} {
		cx, cyy := s*ringR*math.Sin(0.62), cy-ringR*math.Cos(0.62)
		if math.Hypot(x-cx, y-cyy) <= w/2 {
			return 2
		}
	}
	// bar from above the ring to the centre, with round ends
	top, bottom := cy-ringR-0.08, cy-0.04
	if math.Abs(x) <= w/2 && y >= top && y <= bottom {
		return 2
	}
	if math.Hypot(x, y-top) <= w/2 || math.Hypot(x, y-bottom) <= w/2 {
		return 2
	}
	return 1
}

func main() {
	out := "build/appicon.png"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for py := 0; py < size; py++ {
		t := float64(py) / size
		bg := [3]float64{}
		for i := range bg {
			bg[i] = bgTop[i]*(1-t) + bgBottom[i]*t
		}
		for px := 0; px < size; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px)+(float64(sx)+0.5)/ss)/size*2 - 1
					y := (float64(py)+(float64(sy)+0.5)/ss)/size*2 - 1
					switch shape(x, y) {
					case 1:
						r, g, b, a = r+bg[0], g+bg[1], b+bg[2], a+1
					case 2:
						r, g, b, a = r+green[0], g+green[1], b+green[2], a+1
					}
				}
			}
			if a == 0 {
				continue
			}
			img.SetNRGBA(px, py, color.NRGBA{uint8(r / a), uint8(g / a), uint8(b / a), uint8(255 * a / (ss * ss))})
		}
	}
	f, err := os.Create(out)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
}
