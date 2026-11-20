//go:build ignore

// gen_fixture.go generates the test fixture PNG used by the e2e harness.
// Run once: go run gen_fixture.go
// The output (sso_error.png) is committed to the testdata directory.
package main

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
)

func main() {
	// 400x100 white canvas with dark text-block silhouette that encodes
	// known content: "AWS SSO error: sign-in credentials".
	// We use a pixel-art approach: encode a simple pixel pattern that
	// looks like a dialog box so the vision model can describe it, then
	// overlay dark rectangles as a title bar and text lines.
	const w, h = 400, 120
	img := image.NewRGBA(image.Rect(0, 0, w, h))

	// White background.
	draw.Draw(img, img.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)

	// Title bar (dark blue, like an AWS console header).
	titleBar := color.RGBA{R: 35, G: 47, B: 62, A: 255}
	drawRect(img, 0, 0, w, 28, titleBar)

	// "AWS SSO" white text simulation: white rectangles in the title bar.
	white := color.White
	// A B C letters as pixel blocks (simplified).
	drawLetterA(img, 8, 8, white)
	drawLetterW(img, 22, 8, white)
	drawLetterS(img, 38, 8, white)
	// space
	drawLetterS(img, 56, 8, white)
	drawLetterS(img, 70, 8, white)
	drawLetterO(img, 84, 8, white)

	// Warning icon area (orange rectangle).
	orange := color.RGBA{R: 255, G: 153, B: 0, A: 255}
	drawRect(img, 16, 40, 32, 56, orange)

	// Body: simulate "We couldn't verify your sign-in credentials" text.
	// Gray text lines.
	gray := color.RGBA{R: 80, G: 80, B: 80, A: 255}
	lightGray := color.RGBA{R: 180, G: 180, B: 180, A: 255}
	// Line 1: "We couldn't verify your"
	drawRect(img, 50, 44, 300, 50, gray)
	// Line 2: "sign-in credentials"
	drawRect(img, 50, 56, 240, 62, gray)
	// Line 3: thin divider
	drawRect(img, 16, 70, w-16, 72, lightGray)
	// Line 4: "Try signing in again" button-like shape
	btnBlue := color.RGBA{R: 0, G: 115, B: 230, A: 255}
	drawRect(img, 16, 80, 180, 100, btnBlue)

	f, err := os.Create("sso_error.png")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
}

func drawRect(img *image.RGBA, x0, y0, x1, y1 int, c color.Color) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			img.Set(x, y, c)
		}
	}
}

func drawLetterA(img *image.RGBA, x, y int, c color.Color) {
	pts := [][2]int{{2, 0}, {3, 0}, {1, 1}, {4, 1}, {0, 2}, {1, 2}, {2, 2}, {3, 2}, {4, 2}, {5, 2}, {0, 3}, {5, 3}, {0, 4}, {5, 4}}
	for _, p := range pts {
		img.Set(x+p[0], y+p[1], c)
	}
}

func drawLetterW(img *image.RGBA, x, y int, c color.Color) {
	pts := [][2]int{{0, 0}, {5, 0}, {0, 1}, {5, 1}, {0, 2}, {2, 2}, {3, 2}, {5, 2}, {1, 3}, {4, 3}, {2, 4}, {3, 4}}
	for _, p := range pts {
		img.Set(x+p[0], y+p[1], c)
	}
}

func drawLetterS(img *image.RGBA, x, y int, c color.Color) {
	pts := [][2]int{{1, 0}, {2, 0}, {3, 0}, {0, 1}, {0, 2}, {1, 2}, {2, 2}, {3, 2}, {4, 3}, {0, 4}, {1, 4}, {2, 4}, {3, 4}}
	for _, p := range pts {
		img.Set(x+p[0], y+p[1], c)
	}
}

func drawLetterO(img *image.RGBA, x, y int, c color.Color) {
	pts := [][2]int{{1, 0}, {2, 0}, {3, 0}, {0, 1}, {4, 1}, {0, 2}, {4, 2}, {0, 3}, {4, 3}, {1, 4}, {2, 4}, {3, 4}}
	for _, p := range pts {
		img.Set(x+p[0], y+p[1], c)
	}
}
