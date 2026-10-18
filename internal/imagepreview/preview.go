// Package imagepreview 为浏览器生成受限尺寸的 HEIF 与相机 RAW JPEG 预览。
package imagepreview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	maxInputBytes     = 256 << 20
	maxOutputBytes    = 64 << 20
	maxPPMPixels      = 40_000_000
	maxJPEGDecodeSize = 80_000_000
	maxStderrBytes    = 64 << 10
	defaultTimeout    = 20 * time.Second
	maxConcurrent     = 1
	maxQueued         = 32
)

var (
	// ErrDisabled 表示对应格式的系统解码器不可用。
	ErrDisabled = errors.New("image preview decoder is unavailable")
	// ErrUnsupported 表示文件格式不在首期预览范围内。
	ErrUnsupported = errors.New("image preview format is unsupported")
	// ErrInputTooLarge 表示原始文件超过预览输入上限。
	ErrInputTooLarge = errors.New("image preview input exceeds the size limit")
	// ErrOutputTooLarge 表示解码结果超过预览资源上限。
	ErrOutputTooLarge = errors.New("image preview output exceeds the size limit")
	// ErrTimeout 表示解码超过时限。
	ErrTimeout = errors.New("image preview timed out")
	// ErrBusy 表示活动任务和等待队列已满。
	ErrBusy = errors.New("image preview queue is full")
	// ErrInvalidImage 表示解码器没有返回可读图片。
	ErrInvalidImage = errors.New("image preview is invalid")
)

// Size 是缩略图或阅读器预览的最大边长。
type Size string

const (
	Thumbnail Size = "thumb"
	Viewer    Size = "view"
)

// Options 配置外部解码器及其资源上限。
type Options struct {
	HEIFDecoder string
	RAWDecoder  string
	Limiter     string
	Timeout     time.Duration
}

// Generator 在私有临时目录中运行可选的系统解码器。
type Generator struct {
	heifDecoder string
	rawDecoder  string
	limiter     string
	timeout     time.Duration
	semaphore   chan struct{}
	admitted    chan struct{}
}

// New 创建预览生成器；解码器缺失时不影响服务启动。
func New(options Options) *Generator {
	heif := resolveHEIFDecoder(options.HEIFDecoder)
	raw := resolveBinary(options.RAWDecoder, "dcraw_emu")
	limiter := resolveBinary(options.Limiter, "prlimit")
	if runtime.GOOS != "linux" {
		limiter = ""
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Generator{
		heifDecoder: heif,
		rawDecoder:  raw,
		limiter:     limiter,
		timeout:     timeout,
		semaphore:   make(chan struct{}, maxConcurrent),
		admitted:    make(chan struct{}, maxConcurrent+maxQueued),
	}
}

// resolveHEIFDecoder 优先使用 heif-dec，并兼容发行版提供的 heif-convert。
func resolveHEIFDecoder(configured string) string {
	if configured != "" {
		return resolveBinary(configured, "")
	}
	for _, name := range [...]string{"heif-dec", "heif-convert"} {
		if decoder := resolveBinary(name, ""); decoder != "" {
			return decoder
		}
	}
	return ""
}

// Generate 将安全打开的图片流转为浏览器可读的 JPEG。
func (g *Generator) Generate(ctx context.Context, path string, source io.Reader, size Size) ([]byte, error) {
	if g == nil {
		return nil, ErrDisabled
	}
	if source == nil {
		return nil, errors.New("image preview reader is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if runtime.GOOS != "linux" {
		return nil, ErrDisabled
	}
	extension := strings.ToLower(filepath.Ext(path))
	decoder := g.heifDecoder
	if extension == ".dng" || extension == ".cr2" || extension == ".cr3" || extension == ".nef" || extension == ".arw" {
		decoder = g.rawDecoder
	} else if extension != ".heic" && extension != ".heif" {
		return nil, ErrUnsupported
	}
	if decoder == "" || runtime.GOOS == "linux" && g.limiter == "" {
		return nil, ErrDisabled
	}
	maxDimension, ok := previewDimension(size)
	if !ok {
		return nil, ErrUnsupported
	}

	runCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	release, err := g.acquire(runCtx)
	if err != nil {
		if errors.Is(err, ErrBusy) {
			return nil, ErrBusy
		}
		return nil, contextError(ctx, runCtx)
	}
	defer release()

	dir, err := os.MkdirTemp("", "filesage-image-preview-")
	if err != nil {
		return nil, fmt.Errorf("create private image preview directory: %w", err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure image preview directory: %w", err)
	}
	inputPath := filepath.Join(dir, "source"+extension)
	input, err := os.OpenFile(inputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create private image preview input: %w", err)
	}
	inputSize, copyErr := io.Copy(input, io.LimitReader(contextReader{ctx: runCtx, reader: source}, maxInputBytes+1))
	closeErr := input.Close()
	if copyErr != nil {
		return nil, fmt.Errorf("stage image preview input: %w", copyErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close image preview input: %w", closeErr)
	}
	if inputSize > maxInputBytes {
		return nil, ErrInputTooLarge
	}
	if inputSize == 0 {
		return nil, ErrInvalidImage
	}

	if extension == ".heic" || extension == ".heif" {
		return g.decodeHEIF(runCtx, decoder, inputPath, dir, maxDimension)
	}
	return g.decodeRAW(runCtx, decoder, inputPath, maxDimension)
}

// acquire 限制同时运行和排队的预览请求数量。
func (g *Generator) acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case g.admitted <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	select {
	case g.semaphore <- struct{}{}:
		return func() {
			<-g.semaphore
			<-g.admitted
		}, nil
	case <-ctx.Done():
		<-g.admitted
		return nil, ctx.Err()
	}
}

func (g *Generator) decodeHEIF(ctx context.Context, decoder, inputPath, dir string, maxDimension int) ([]byte, error) {
	outputPath := filepath.Join(dir, "preview.jpg")
	cmd, err := g.command(ctx, decoder, "-q", "82", inputPath, outputPath)
	if err != nil {
		return nil, err
	}
	if err := cmd.Run(); err != nil {
		return nil, processError(ctx, err)
	}
	info, err := os.Stat(outputPath)
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrInvalidImage
	}
	if info.Size() <= 0 || info.Size() > maxOutputBytes {
		return nil, ErrOutputTooLarge
	}
	file, err := os.Open(outputPath)
	if err != nil {
		return nil, fmt.Errorf("open HEIF decoder output: %w", err)
	}
	defer file.Close()
	return jpegPreview(file, maxDimension)
}

func (g *Generator) decodeRAW(ctx context.Context, decoder, inputPath string, maxDimension int) ([]byte, error) {
	cmd, err := g.command(ctx, decoder, "-h", "-w", "-Z", "-", inputPath)
	if err != nil {
		return nil, err
	}
	output := &limitedBuffer{limit: maxOutputBytes, terminate: true}
	output.command = cmd
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		if output.exceeded {
			return nil, ErrOutputTooLarge
		}
		return nil, processError(ctx, err)
	}
	if output.exceeded {
		return nil, ErrOutputTooLarge
	}
	decoded, err := decodePPM(output.data.Bytes())
	if err != nil {
		return nil, err
	}
	return encodeJPEG(decoded, maxDimension)
}

func (g *Generator) command(ctx context.Context, decoder string, args ...string) (*exec.Cmd, error) {
	if decoder == "" {
		return nil, ErrDisabled
	}
	if runtime.GOOS == "linux" {
		if g.limiter == "" {
			return nil, ErrDisabled
		}
		limited := []string{"--as=1073741824", "--cpu=20", "--fsize=" + strconv.FormatInt(maxOutputBytes, 10), "--", decoder}
		args = append(limited, args...)
		decoder = g.limiter
	}
	cmd := exec.CommandContext(ctx, decoder, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "LANG=C"}
	cmd.Stderr = &limitedBuffer{limit: maxStderrBytes}
	configureProcess(cmd)
	return cmd, nil
}

func resolveBinary(configured, fallback string) string {
	if configured == "" {
		configured = fallback
	}
	path, err := exec.LookPath(configured)
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	return resolved
}

func previewDimension(size Size) (int, bool) {
	switch size {
	case Thumbnail:
		return 480, true
	case Viewer:
		return 2400, true
	default:
		return 0, false
	}
}

func contextError(parent, run context.Context) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(run.Err(), context.DeadlineExceeded) {
		return ErrTimeout
	}
	return run.Err()
}

func processError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrTimeout
		}
		return ctx.Err()
	}
	return fmt.Errorf("image decoder failed: %w", err)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(data []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(data)
}

type limitedBuffer struct {
	data      bytes.Buffer
	limit     int
	exceeded  bool
	terminate bool
	command   *exec.Cmd
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	remaining := buffer.limit - buffer.data.Len()
	if remaining <= 0 {
		buffer.exceeded = true
		if buffer.terminate && buffer.command != nil {
			_ = killProcessTree(buffer.command)
		}
		return len(data), nil
	}
	if len(data) > remaining {
		_, _ = buffer.data.Write(data[:remaining])
		buffer.exceeded = true
		if buffer.terminate && buffer.command != nil {
			_ = killProcessTree(buffer.command)
		}
		return len(data), nil
	}
	return buffer.data.Write(data)
}

type ppmImage struct {
	pixels []byte
	bounds image.Rectangle
}

func (ppm *ppmImage) ColorModel() color.Model { return color.RGBAModel }
func (ppm *ppmImage) Bounds() image.Rectangle { return ppm.bounds }
func (ppm *ppmImage) At(x, y int) color.Color {
	if !image.Pt(x, y).In(ppm.bounds) {
		return color.RGBA{}
	}
	index := ((y-ppm.bounds.Min.Y)*ppm.bounds.Dx() + (x - ppm.bounds.Min.X)) * 3
	return color.RGBA{R: ppm.pixels[index], G: ppm.pixels[index+1], B: ppm.pixels[index+2], A: 0xff}
}

// decodePPM 只接受 dcraw_emu 输出的 8 位 P6 像素流。
func decodePPM(data []byte) (image.Image, error) {
	reader := ppmReader{data: data}
	magic, ok := reader.token()
	if !ok || magic != "P6" {
		return nil, ErrInvalidImage
	}
	widthValue, widthOK := reader.token()
	heightValue, heightOK := reader.token()
	maxValue, maxOK := reader.token()
	if !widthOK || !heightOK || !maxOK || maxValue != "255" {
		return nil, ErrInvalidImage
	}
	width, widthErr := strconv.Atoi(widthValue)
	height, heightErr := strconv.Atoi(heightValue)
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 || int64(width)*int64(height) > maxPPMPixels {
		return nil, ErrInvalidImage
	}
	if reader.offset >= len(data) || data[reader.offset] != '\n' && data[reader.offset] != '\r' && data[reader.offset] != ' ' && data[reader.offset] != '\t' {
		return nil, ErrInvalidImage
	}
	if data[reader.offset] == '\r' && reader.offset+1 < len(data) && data[reader.offset+1] == '\n' {
		reader.offset += 2
	} else {
		reader.offset++
	}
	pixelBytes := int64(width) * int64(height) * 3
	if pixelBytes > maxOutputBytes {
		return nil, ErrOutputTooLarge
	}
	if pixelBytes > int64(len(data)-reader.offset) {
		return nil, ErrInvalidImage
	}
	return &ppmImage{pixels: data[reader.offset : reader.offset+int(pixelBytes)], bounds: image.Rect(0, 0, width, height)}, nil
}

type ppmReader struct {
	data   []byte
	offset int
}

func (reader *ppmReader) token() (string, bool) {
	for reader.offset < len(reader.data) {
		value := reader.data[reader.offset]
		if value == '#' {
			for reader.offset < len(reader.data) && reader.data[reader.offset] != '\n' {
				reader.offset++
			}
			continue
		}
		if value != ' ' && value != '\t' && value != '\r' && value != '\n' {
			break
		}
		reader.offset++
	}
	start := reader.offset
	for reader.offset < len(reader.data) {
		value := reader.data[reader.offset]
		if value == ' ' || value == '\t' || value == '\r' || value == '\n' || value == '#' {
			break
		}
		reader.offset++
	}
	if start == reader.offset {
		return "", false
	}
	return string(reader.data[start:reader.offset]), true
}

func jpegPreview(source io.Reader, maxDimension int) ([]byte, error) {
	seeker, ok := source.(io.Seeker)
	if !ok {
		return nil, ErrInvalidImage
	}
	config, err := jpeg.DecodeConfig(io.LimitReader(source, maxOutputBytes+1))
	if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > maxJPEGDecodeSize {
		return nil, ErrInvalidImage
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		return nil, ErrInvalidImage
	}
	decoded, err := jpeg.Decode(source)
	if err != nil {
		return nil, ErrInvalidImage
	}
	return encodeJPEG(decoded, maxDimension)
}

func encodeJPEG(source image.Image, maxDimension int) ([]byte, error) {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 || int64(width)*int64(height) > maxJPEGDecodeSize {
		return nil, ErrInvalidImage
	}
	if maxDimension <= 0 {
		return nil, ErrUnsupported
	}
	scale := float64(maxDimension) / float64(max(width, height))
	if scale > 1 {
		scale = 1
	}
	outputWidth := max(1, int(float64(width)*scale))
	outputHeight := max(1, int(float64(height)*scale))
	view := scaledImage{source: source, bounds: image.Rect(0, 0, outputWidth, outputHeight)}
	output := &limitedBuffer{limit: maxOutputBytes}
	if err := jpeg.Encode(output, view, &jpeg.Options{Quality: 82}); err != nil {
		if output.exceeded {
			return nil, ErrOutputTooLarge
		}
		return nil, fmt.Errorf("encode image preview: %w", err)
	}
	if output.exceeded {
		return nil, ErrOutputTooLarge
	}
	return output.data.Bytes(), nil
}

type scaledImage struct {
	source image.Image
	bounds image.Rectangle
}

func (scaled scaledImage) ColorModel() color.Model { return color.RGBAModel }
func (scaled scaledImage) Bounds() image.Rectangle { return scaled.bounds }
func (scaled scaledImage) At(x, y int) color.Color {
	if !image.Pt(x, y).In(scaled.bounds) {
		return color.RGBA{}
	}
	sourceBounds := scaled.source.Bounds()
	sx := float64(x)*float64(sourceBounds.Dx())/float64(scaled.bounds.Dx()) + float64(sourceBounds.Min.X)
	sy := float64(y)*float64(sourceBounds.Dy())/float64(scaled.bounds.Dy()) + float64(sourceBounds.Min.Y)
	x0, y0 := int(sx), int(sy)
	x1, y1 := min(x0+1, sourceBounds.Max.X-1), min(y0+1, sourceBounds.Max.Y-1)
	fx, fy := sx-float64(x0), sy-float64(y0)
	c00 := color.RGBAModel.Convert(scaled.source.At(x0, y0)).(color.RGBA)
	c10 := color.RGBAModel.Convert(scaled.source.At(x1, y0)).(color.RGBA)
	c01 := color.RGBAModel.Convert(scaled.source.At(x0, y1)).(color.RGBA)
	c11 := color.RGBAModel.Convert(scaled.source.At(x1, y1)).(color.RGBA)
	return color.RGBA{
		R: interpolate(c00.R, c10.R, c01.R, c11.R, fx, fy),
		G: interpolate(c00.G, c10.G, c01.G, c11.G, fx, fy),
		B: interpolate(c00.B, c10.B, c01.B, c11.B, fx, fy),
		A: 0xff,
	}
}

func interpolate(a, b, c, d uint8, x, y float64) uint8 {
	top := float64(a)*(1-x) + float64(b)*x
	bottom := float64(c)*(1-x) + float64(d)*x
	return uint8(top*(1-y) + bottom*y + 0.5)
}
