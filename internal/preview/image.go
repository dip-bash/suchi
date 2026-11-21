// Package preview turns clipboard images and files into something a terminal
// can show: it decodes images, fits them into a cell box keeping their
// aspect ratio, and renders them with the best protocol the terminal speaks
// (kitty, iTerm2, sixel) or as colour half-block "ANSI art" everywhere else.
package preview

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"

	// Decoders registered for image.Decode.
	_ "image/gif"
	_ "image/jpeg"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	xdraw "golang.org/x/image/draw"
)

// MaxPixels refuses decompression bombs: a 30 KB PNG can claim to be gigapixels.
const MaxPixels = 64 << 20

// background is what transparent pixels are composited onto.
var background = color.NRGBA{R: 0x1e, G: 0x1e, B: 0x2e, A: 0xff}

// Decode decodes an image of any supported format (png, jpeg, gif, webp, bmp, tiff).
func Decode(data []byte) (img image.Image, format string, err error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("not a supported image: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > MaxPixels {
		return nil, "", errors.New("image is too large to preview")
	}
	img, _, err = image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode image: %w", err)
	}
	return img, format, nil
}

// Config returns the dimensions and format without decoding the pixels.
func Config(data []byte) (w, h int, format string, err error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, "", err
	}
	return cfg.Width, cfg.Height, format, nil
}

// maxUpscale stops a tiny icon from being blown up to fill the whole popup.
const maxUpscale = 4.0

// Fit returns the size in cells of an imgW x imgH image shown inside a
// maxCols x maxRows box, keeping its aspect ratio. cellW and cellH are the
// pixel size of one cell (for half-block rendering use 1 and 2).
func Fit(imgW, imgH, maxCols, maxRows, cellW, cellH int) (cols, rows int) {
	if imgW <= 0 || imgH <= 0 || maxCols <= 0 || maxRows <= 0 || cellW <= 0 || cellH <= 0 {
		return 1, 1
	}
	wc := float64(imgW) / float64(cellW) // size in cells at 1 pixel per image pixel
	hc := float64(imgH) / float64(cellH)
	scale := math.Min(math.Min(float64(maxCols)/wc, float64(maxRows)/hc), maxUpscale)
	cols = int(math.Round(wc * scale))
	rows = int(math.Round(hc * scale))
	return max(1, min(cols, maxCols)), max(1, min(rows, maxRows))
}

// Resize scales img to exactly w x h pixels, compositing transparency onto the background.
func Resize(img image.Image, w, h int) *image.NRGBA {
	w, h = max(w, 1), max(h, 1)
	flat := image.NewNRGBA(img.Bounds())
	draw.Draw(flat, flat.Bounds(), &image.Uniform{C: background}, image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), img, img.Bounds().Min, draw.Over)
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), flat, flat.Bounds(), xdraw.Src, nil)
	return dst
}

// EncodePNG encodes an image as PNG.
func EncodePNG(img image.Image) ([]byte, error) {
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
