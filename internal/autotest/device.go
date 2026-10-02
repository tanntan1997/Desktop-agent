package autotest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"math"

	"github.com/ababank/mobile-device-agent/internal/hub"
)

// Device is what tests drive. Coordinates are screenshot pixels.
type Device interface {
	Screenshot(ctx context.Context) ([]byte, error) // PNG (or JPEG)
	Source(ctx context.Context) (string, error)
	Tap(ctx context.Context, x, y int) error
	Swipe(ctx context.Context, x1, y1, x2, y2, durationMs int) error
	Text(ctx context.Context, text string) error
	Key(ctx context.Context, key string) error
	Launch(ctx context.Context, appID string) error
	Terminate(ctx context.Context, appID string) error
}

// hubDevice drives a device through its agent's tunnel.
type hubDevice struct {
	agent  *hub.Agent
	serial string
}

func (d *hubDevice) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	params["serial"] = d.serial
	return d.agent.Call(ctx, method, params)
}

func (d *hubDevice) Screenshot(ctx context.Context) ([]byte, error) {
	res, err := d.call(ctx, "device.screenshot", map[string]any{})
	if err != nil {
		return nil, err
	}
	var shot struct{ Data string }
	if err := json.Unmarshal(res, &shot); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(shot.Data)
}

func (d *hubDevice) Source(ctx context.Context) (string, error) {
	res, err := d.call(ctx, "device.source", map[string]any{})
	if err != nil {
		return "", err
	}
	var src struct{ Source string }
	err = json.Unmarshal(res, &src)
	return src.Source, err
}

func (d *hubDevice) Tap(ctx context.Context, x, y int) error {
	_, err := d.call(ctx, "device.tap", map[string]any{"x": x, "y": y})
	return err
}

func (d *hubDevice) Swipe(ctx context.Context, x1, y1, x2, y2, ms int) error {
	_, err := d.call(ctx, "device.swipe", map[string]any{"x1": x1, "y1": y1, "x2": x2, "y2": y2, "durationMs": ms})
	return err
}

func (d *hubDevice) Text(ctx context.Context, text string) error {
	_, err := d.call(ctx, "device.text", map[string]any{"text": text})
	return err
}

func (d *hubDevice) Key(ctx context.Context, key string) error {
	_, err := d.call(ctx, "device.key", map[string]any{"key": key})
	return err
}

func (d *hubDevice) Launch(ctx context.Context, appID string) error {
	_, err := d.call(ctx, "app.launch", map[string]any{"appId": appID})
	return err
}

func (d *hubDevice) Terminate(ctx context.Context, appID string) error {
	_, err := d.call(ctx, "app.terminate", map[string]any{"appId": appID})
	return err
}

// Model image limits: long edge <= 1568 px and about 1.15 megapixels, beyond
// which the API downsizes anyway.
const (
	maxImageEdge   = 1568
	maxImagePixels = 1_150_000
)

// shrink decodes a screenshot and returns it as JPEG scaled to fit the model's
// image limits, with k = output/input scale and the original size.
func shrink(img []byte) (out []byte, k float64, w, h int, err error) {
	src, _, err := image.Decode(bytes.NewReader(img))
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("decode screenshot: %w", err)
	}
	b := src.Bounds()
	w, h = b.Dx(), b.Dy()
	k = 1.0
	if long := max(w, h); long > maxImageEdge {
		k = float64(maxImageEdge) / float64(long)
	}
	if px := float64(w*h) * k * k; px > maxImagePixels {
		k *= math.Sqrt(maxImagePixels / px)
	}
	dw, dh := max(int(float64(w)*k), 1), max(int(float64(h)*k), 1)
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	boxScale(dst, src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 80}); err != nil {
		return nil, 0, 0, 0, err
	}
	return buf.Bytes(), float64(dw) / float64(w), w, h, nil
}

// boxScale downsamples src into dst by averaging the source pixels that fall
// into each destination pixel (good quality for large reductions).
func boxScale(dst *image.RGBA, src image.Image) {
	sb, db := src.Bounds(), dst.Bounds()
	sw, sh, dw, dh := sb.Dx(), sb.Dy(), db.Dx(), db.Dy()
	// Screenshots decode to NRGBA/RGBA; read their pixels directly instead of
	// through the per-pixel interface.
	var pix []uint8
	var stride int
	switch s := src.(type) {
	case *image.NRGBA:
		pix, stride = s.Pix, s.Stride
	case *image.RGBA:
		pix, stride = s.Pix, s.Stride
	}
	for dy := 0; dy < dh; dy++ {
		y0, y1 := dy*sh/dh, max((dy+1)*sh/dh, dy*sh/dh+1)
		for dx := 0; dx < dw; dx++ {
			x0, x1 := dx*sw/dw, max((dx+1)*sw/dw, dx*sw/dw+1)
			var r, g, b, n uint32
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					if pix != nil {
						i := y*stride + x*4
						r, g, b = r+uint32(pix[i]), g+uint32(pix[i+1]), b+uint32(pix[i+2])
					} else {
						cr, cg, cb, _ := src.At(sb.Min.X+x, sb.Min.Y+y).RGBA()
						r, g, b = r+cr>>8, g+cg>>8, b+cb>>8
					}
					n++
				}
			}
			i := dst.PixOffset(dx, dy)
			dst.Pix[i+0] = uint8(r / n)
			dst.Pix[i+1] = uint8(g / n)
			dst.Pix[i+2] = uint8(b / n)
			dst.Pix[i+3] = 0xff
		}
	}
}
