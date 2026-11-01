package vision

import (
	"bytes"
	"encoding/binary"
	"image"
)

// A phone photo carries EXIF: where it was taken (GPS), the camera, and the orientation the picture is
// meant to be shown in. None of that goes to a model: a JPEG or PNG with metadata is decoded and
// re-encoded (the standard encoders write none), and the EXIF orientation is applied on the way, so a
// sideways phone photo reaches the model upright.

// metaOf reports whether a JPEG or PNG carries metadata (EXIF, XMP, IPTC, text chunks) and the EXIF
// orientation (1, the default, when it has none or an unknown one).
func metaOf(mime string, data []byte) (orientation int, has bool) {
	switch mime {
	case "image/jpeg":
		return jpegMeta(data)
	case "image/png":
		return 1, pngMeta(data)
	}
	return 1, false
}

// jpegMeta walks a JPEG's segments up to the image data: APP1 (EXIF, XMP), APP13 (IPTC) and COM count as
// metadata.
func jpegMeta(data []byte) (orientation int, has bool) {
	orientation = 1
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return orientation, false
	}
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			return orientation, has
		}
		marker := data[i+1]
		if marker == 0xFF { // fill byte
			i++
			continue
		}
		if marker == 0xDA || marker == 0xD9 { // start of scan, end of image: no more metadata
			return orientation, has
		}
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			i += 2
			continue
		}
		n := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if n < 2 || i+2+n > len(data) {
			return orientation, has
		}
		seg := data[i+4 : i+2+n]
		switch marker {
		case 0xE1: // APP1
			has = true
			if bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
				if o := exifOrientation(seg[6:]); o >= 1 && o <= 8 {
					orientation = o
				}
			}
		case 0xED, 0xFE: // APP13 (Photoshop, IPTC), comment
			has = true
		}
		i += 2 + n
	}
	return orientation, has
}

// exifOrientation reads tag 0x0112 of IFD0 from a TIFF block; 0 when it is not there.
func exifOrientation(t []byte) int {
	if len(t) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	off := int(bo.Uint32(t[4:8]))
	if off < 8 || off+2 > len(t) {
		return 0
	}
	count := int(bo.Uint16(t[off : off+2]))
	for k := 0; k < count; k++ {
		e := off + 2 + 12*k
		if e+12 > len(t) {
			return 0
		}
		if bo.Uint16(t[e:e+2]) == 0x0112 && bo.Uint16(t[e+2:e+4]) == 3 { // orientation, SHORT
			return int(bo.Uint16(t[e+8 : e+10]))
		}
	}
	return 0
}

// pngMeta reports a PNG with an eXIf, tEXt, zTXt, iTXt or tIME chunk before its image data.
func pngMeta(data []byte) bool {
	if len(data) < 8 {
		return false
	}
	for i := 8; i+8 <= len(data); {
		n := int(binary.BigEndian.Uint32(data[i : i+4]))
		typ := string(data[i+4 : i+8])
		switch typ {
		case "eXIf", "tEXt", "zTXt", "iTXt", "tIME":
			return true
		case "IDAT", "IEND":
			return false
		}
		if n < 0 || i+12+n > len(data) {
			return false
		}
		i += 12 + n
	}
	return false
}

// orient turns a picture as its EXIF orientation says (2–8; anything else returns it as it is).
func orient(src *image.RGBA, o int) *image.RGBA {
	if o < 2 || o > 8 {
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for sy := 0; sy < h; sy++ {
		for sx := 0; sx < w; sx++ {
			var x, y int
			switch o {
			case 2: // mirrored
				x, y = w-1-sx, sy
			case 3: // upside down
				x, y = w-1-sx, h-1-sy
			case 4: // mirrored upside down
				x, y = sx, h-1-sy
			case 5: // transposed
				x, y = sy, sx
			case 6: // turned a quarter clockwise to show
				x, y = h-1-sy, sx
			case 7: // transversed
				x, y = h-1-sy, w-1-sx
			case 8: // turned a quarter counter-clockwise to show
				x, y = sy, w-1-sx
			}
			so, do := sy*src.Stride+sx*4, y*dst.Stride+x*4
			copy(dst.Pix[do:do+4], src.Pix[so:so+4])
		}
	}
	return dst
}
