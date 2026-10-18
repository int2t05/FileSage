// 本文件验证像素解码与预览尺寸处理。
package imagepreview

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestNewResolvesHEIFDecoderNames 验证发行版命令名、优先级和显式配置。
func TestNewResolvesHEIFDecoderNames(t *testing.T) {
	t.Run("prefers heif-dec", func(t *testing.T) {
		path := t.TempDir()
		decoderPath := writeLookupBinary(t, path, "heif-dec")
		writeLookupBinary(t, path, "heif-convert")
		t.Setenv("PATH", path)
		if got := New(Options{}).heifDecoder; got != decoderPath {
			t.Fatalf("resolved decoder = %q, want %q", got, decoderPath)
		}
	})

	t.Run("falls back to heif-convert", func(t *testing.T) {
		path := t.TempDir()
		decoderPath := writeLookupBinary(t, path, "heif-convert")
		t.Setenv("PATH", path)
		if got := New(Options{}).heifDecoder; got != decoderPath {
			t.Fatalf("resolved decoder = %q, want %q", got, decoderPath)
		}
	})

	t.Run("uses configured decoder", func(t *testing.T) {
		path := t.TempDir()
		customPath := writeLookupBinary(t, path, "custom-heif")
		writeLookupBinary(t, path, "heif-dec")
		t.Setenv("PATH", path)
		if got := New(Options{HEIFDecoder: customPath}).heifDecoder; got != customPath {
			t.Fatalf("resolved decoder = %q, want %q", got, customPath)
		}
	})

	t.Run("remains unavailable without a decoder", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if got := New(Options{}).heifDecoder; got != "" {
			t.Fatalf("resolved decoder = %q, want unavailable", got)
		}
	})
}

// writeLookupBinary 创建仅用于路径发现测试的空可执行文件。
func writeLookupBinary(t *testing.T, directory, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte("test"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestGeneratorAdmissionBoundsQueue 验证预览队列有上限且取消后释放名额。
func TestGeneratorAdmissionBoundsQueue(t *testing.T) {
	if got, want := cap(New(Options{}).admitted), maxConcurrent+maxQueued; got != want {
		t.Fatalf("production admission capacity = %d, want %d", got, want)
	}
	generator := &Generator{
		semaphore: make(chan struct{}, 1),
		admitted:  make(chan struct{}, 2),
	}
	generator.semaphore <- struct{}{}
	generator.admitted <- struct{}{}
	generator.admitted <- struct{}{}
	if _, err := generator.acquire(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("full queue error = %v, want ErrBusy", err)
	}

	generator = &Generator{
		semaphore: make(chan struct{}, 1),
		admitted:  make(chan struct{}, 1),
	}
	generator.semaphore <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := generator.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled queue error = %v, want context deadline", err)
	}
	if len(generator.admitted) != 0 {
		t.Fatalf("queue admission leaked after cancellation: %d", len(generator.admitted))
	}
}

// TestDecodePPMReadsP6Pixels 验证 P6 像素读取。
func TestDecodePPMReadsP6Pixels(t *testing.T) {
	data := append([]byte("P6\n2 1\n255\n"), 10, 20, 30, 240, 250, 255)
	decoded, err := decodePPM(data)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds() != image.Rect(0, 0, 2, 1) {
		t.Fatalf("bounds = %v", decoded.Bounds())
	}
	first := color.RGBAModel.Convert(decoded.At(0, 0)).(color.RGBA)
	second := color.RGBAModel.Convert(decoded.At(1, 0)).(color.RGBA)
	if first != (color.RGBA{R: 10, G: 20, B: 30, A: 255}) || second != (color.RGBA{R: 240, G: 250, B: 255, A: 255}) {
		t.Fatalf("decoded pixels = %#v, %#v", first, second)
	}
}

// TestDecodePPMRejectsInvalidHeadersAndTruncatedPixels 验证非法头部和截断像素被拒绝。
func TestDecodePPMRejectsInvalidHeadersAndTruncatedPixels(t *testing.T) {
	for _, input := range [][]byte{
		[]byte("P3\n1 1\n255\n0 0 0\n"),
		[]byte("P6\n0 1\n255\n"),
		[]byte("P6\n1 1\n1\n0 0 0"),
		[]byte("P6\n6400 6400\n255\n"),
		[]byte("P6\n1 1\n255\n\x00\x01"),
	} {
		if _, err := decodePPM(input); !errors.Is(err, ErrInvalidImage) {
			t.Fatalf("decodePPM(%q) error = %v, want ErrInvalidImage", input, err)
		}
	}
}

// TestEncodeJPEGResizesAndDoesNotUpscale 验证缩放上限和小图不放大。
func TestEncodeJPEGResizesAndDoesNotUpscale(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 4; x++ {
			source.Set(x, y, color.RGBA{R: uint8(x * 60), G: uint8(y * 120), B: 30, A: 255})
		}
	}

	for _, test := range []struct {
		name         string
		maxDimension int
		want         image.Point
	}{
		{name: "downscale", maxDimension: 2, want: image.Pt(2, 1)},
		{name: "no upscale", maxDimension: 10, want: image.Pt(4, 2)},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := encodeJPEG(source, test.maxDimension)
			if err != nil {
				t.Fatal(err)
			}
			config, err := jpeg.DecodeConfig(bytes.NewReader(encoded))
			if err != nil {
				t.Fatal(err)
			}
			if got := image.Pt(config.Width, config.Height); got != test.want {
				t.Fatalf("JPEG size = %v, want %v", got, test.want)
			}
		})
	}
}
