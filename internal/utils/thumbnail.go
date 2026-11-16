package utils

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // registers the GIF decoder; a GIF gives its first frame
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"os"

	_ "golang.org/x/image/bmp" // registers the BMP decoder
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers the WebP decoder
)

// ThumbDir holds the thumbnails, under the upload dir. A thumbnail has the storage key of its picture as its name, so
// the orphan sweep can check it against the files table. The sweep of the upload dir reads only regular files at its
// top, so it never sees this dir.
const ThumbDir = "thumbs"

// thumbJPEGQuality keeps a 256px photo near 20 KB with no visible loss at that size.
const thumbJPEGQuality = 80

// exifHeadSize is how much of a JPEG the orientation reader looks at. The EXIF segment comes first and is at most 64 KB.
const exifHeadSize = 128 << 10

// ErrNoThumbnail means the bytes are not a picture this server can shrink, or the picture is over the pixel cap.
var ErrNoThumbnail = errors.New("no thumbnail")

// MakeThumbnail writes a small copy of the picture under key in dir to ThumbDir, under the same key. Its long edge is
// at most edge pixels, and it is never bigger than the picture. A picture over maxPixels is refused before it decodes,
// so a huge picture cannot take all memory. An opaque picture becomes a JPEG, and one with transparency a PNG. A JPEG
// turns by its EXIF orientation, the same as in the browser. A missing picture gives ErrNoStoredFile.
func MakeThumbnail(dir, key string, edge int, maxPixels int64) error {
	uploads, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("failed to open upload dir: %w", err)
	}
	defer func() { _ = uploads.Close() }()

	src, err := uploads.Open(key)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNoStoredFile
	}
	if err != nil {
		return fmt.Errorf("failed to open stored file: %w", err)
	}
	defer func() { _ = src.Close() }()

	cfg, format, err := image.DecodeConfig(src)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNoThumbnail, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return fmt.Errorf("%w: the picture is %dx%d", ErrNoThumbnail, cfg.Width, cfg.Height)
	}

	orientation := 1
	if format == "jpeg" {
		head := make([]byte, exifHeadSize)
		n, err := src.ReadAt(head, 0)
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("failed to read stored file: %w", err)
		}
		orientation = jpegOrientation(head[:n])
	}

	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("failed to read stored file: %w", err)
	}
	img, _, err := image.Decode(src)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNoThumbnail, err)
	}

	w, h := cfg.Width, cfg.Height
	if w > edge || h > edge {
		if w >= h {
			w, h = edge, max(1, h*edge/w)
		} else {
			w, h = max(1, w*edge/h), edge
		}
	}
	small := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.BiLinear.Scale(small, small.Bounds(), img, img.Bounds(), draw.Src, nil)
	small = orient(small, orientation)

	var buf bytes.Buffer
	if small.Opaque() {
		err = jpeg.Encode(&buf, small, &jpeg.Options{Quality: thumbJPEGQuality})
	} else {
		err = png.Encode(&buf, small)
	}
	if err != nil {
		return fmt.Errorf("failed to encode thumbnail: %w", err)
	}

	if err := uploads.Mkdir(ThumbDir, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("failed to create thumbnail dir: %w", err)
	}
	thumbs, err := uploads.OpenRoot(ThumbDir)
	if err != nil {
		return fmt.Errorf("failed to open thumbnail dir: %w", err)
	}
	defer func() { _ = thumbs.Close() }()

	_, err = writeStoredFile(thumbs, key, &buf, int64(buf.Len()))
	return err
}

// jpegOrientation returns the EXIF orientation of a JPEG from its first bytes, from 1 to 8. A JPEG with no tag, or a
// bad one, gives 1. It walks the segments up to the image data, finds the EXIF segment, and reads the orientation tag
// (0x0112) from its first directory.
func jpegOrientation(head []byte) int {
	if len(head) < 4 || head[0] != 0xFF || head[1] != 0xD8 {
		return 1
	}
	for i := 2; i+4 <= len(head); {
		if head[i] != 0xFF {
			return 1
		}
		marker := head[i+1]
		if marker == 0xFF {
			i++ // a fill byte before the marker
			continue
		}
		if marker == 0xDA || marker == 0xD9 {
			return 1 // the image data starts, and no EXIF segment came before it
		}
		end := i + 2 + int(binary.BigEndian.Uint16(head[i+2:]))
		if end <= i+4 || end > len(head) {
			return 1
		}
		seg := head[i+4 : end]
		i = end
		if marker != 0xE1 || !bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
			continue
		}

		tiff := seg[6:]
		if len(tiff) < 8 {
			return 1
		}
		var order binary.ByteOrder
		switch string(tiff[:2]) {
		case "II":
			order = binary.LittleEndian
		case "MM":
			order = binary.BigEndian
		default:
			return 1
		}
		ifd := int64(order.Uint32(tiff[4:]))
		if ifd < 8 || ifd+2 > int64(len(tiff)) {
			return 1
		}
		count := int(order.Uint16(tiff[ifd:]))
		for e := int(ifd) + 2; count > 0 && e+12 <= len(tiff); e, count = e+12, count-1 {
			const orientationTag, shortType = 0x0112, 3
			if order.Uint16(tiff[e:]) != orientationTag || order.Uint16(tiff[e+2:]) != shortType {
				continue
			}
			if o := int(order.Uint16(tiff[e+8:])); o >= 1 && o <= 8 {
				return o
			}
			return 1
		}
		return 1
	}
	return 1
}

// orient turns and flips img so it shows as its EXIF orientation says. Orientations 5 to 8 swap the width and height.
func orient(img *image.RGBA, orientation int) *image.RGBA {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	var source func(x, y int) (int, int)
	switch orientation {
	case 2: // flipped left to right
		source = func(x, y int) (int, int) { return w - 1 - x, y }
	case 3: // turned 180 degrees
		source = func(x, y int) (int, int) { return w - 1 - x, h - 1 - y }
	case 4: // flipped top to bottom
		source = func(x, y int) (int, int) { return x, h - 1 - y }
	case 5: // flipped left to right, then turned 90 degrees counterclockwise
		source = func(x, y int) (int, int) { return y, x }
	case 6: // turned 90 degrees clockwise
		source = func(x, y int) (int, int) { return y, h - 1 - x }
	case 7: // flipped left to right, then turned 90 degrees clockwise
		source = func(x, y int) (int, int) { return w - 1 - y, h - 1 - x }
	case 8: // turned 90 degrees counterclockwise
		source = func(x, y int) (int, int) { return w - 1 - y, x }
	default:
		return img
	}

	dw, dh := w, h
	if orientation >= 5 {
		dw, dh = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := range dh {
		for x := range dw {
			sx, sy := source(x, y)
			copy(out.Pix[out.PixOffset(x, y):][:4], img.Pix[img.PixOffset(sx, sy):][:4])
		}
	}
	return out
}
