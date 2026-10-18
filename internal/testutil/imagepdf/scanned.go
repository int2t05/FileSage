//go:build linux

// Package imagepdf 提供 PDF 测试用的纯图像页面。
package imagepdf

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
)

// FromPDF 将第一页栅格化后嵌入新 PDF，以移除可提取的文字层。
func FromPDF(ctx context.Context, source []byte) ([]byte, error) {
	directory, err := os.MkdirTemp("", "filesage-image-pdf-test-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	pdfPath := filepath.Join(directory, "source.pdf")
	prefix := filepath.Join(directory, "page")
	if err := os.WriteFile(pdfPath, source, 0o600); err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, "pdftoppm", "-q", "-f", "1", "-l", "1", "-gray", "-png", "-scale-to", "1600", pdfPath, prefix)
	if output, err := command.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("render image PDF fixture: %w: %s", err, output)
	}
	file, err := os.Open(prefix + "-1.png")
	if err != nil {
		return nil, err
	}
	page, decodeErr := png.Decode(file)
	closeErr := file.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil {
		return nil, err
	}
	width, height := page.Bounds().Dx(), page.Bounds().Dy()
	if width < 1 || height < 1 || int64(width)*int64(height) > 1600*1600 {
		return nil, errors.New("rendered page dimensions are invalid")
	}
	pixels := make([]byte, width*height)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			pixels[y*width+x] = color.GrayModel.Convert(page.At(x, y)).(color.Gray).Y
		}
	}
	var imageData bytes.Buffer
	compressed := zlib.NewWriter(&imageData)
	if _, err := compressed.Write(pixels); err != nil {
		return nil, err
	}
	if err := compressed.Close(); err != nil {
		return nil, err
	}
	return encodeImagePDF(width, height, imageData.Bytes()), nil
}

// encodeImagePDF 用最小对象表生成单页图像 PDF。
func encodeImagePDF(width, height int, pixels []byte) []byte {
	imageObject := new(bytes.Buffer)
	fmt.Fprintf(imageObject, "<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceGray /BitsPerComponent 8 /Filter /FlateDecode /Length %d >>\nstream\n", width, height, len(pixels))
	imageObject.Write(pixels)
	imageObject.WriteString("\nendstream")
	content := []byte(fmt.Sprintf("q %d 0 0 %d 0 0 cm /Im0 Do Q\n", width, height))
	contentObject := []byte(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
	objects := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d %d] /Resources << /XObject << /Im0 4 0 R >> >> /Contents 5 0 R >>", width, height)),
		imageObject.Bytes(),
		contentObject,
	}
	output := bytes.NewBufferString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	offsets := make([]int, len(objects)+1)
	for index, object := range objects {
		id := index + 1
		offsets[id] = output.Len()
		fmt.Fprintf(output, "%d 0 obj\n", id)
		output.Write(object)
		output.WriteString("\nendobj\n")
	}
	xrefOffset := output.Len()
	fmt.Fprintf(output, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, offset := range offsets[1:] {
		fmt.Fprintf(output, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xrefOffset)
	return output.Bytes()
}
