// Package wim reads the images a Windows image file (install.wim, or an
// install.esd) describes: each image's index, name, and edition, from the
// XML resource the file's header points to. It reads no image's files, and
// leaves choosing among them to its caller.
package wim

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Image is one image in the file.
type Image struct {
	Index int
	Name  string
	// EditionID is Windows' edition (Professional, Core, ...), where the
	// image records it.
	EditionID string
}

// headerSize is the WIM header's size; maxXML bounds the XML resource
// against a corrupt or hostile file.
const (
	headerSize = 208
	maxXML     = 16 << 20
)

// Images reads the images r describes, in index order.
func Images(r io.ReaderAt) ([]Image, error) {
	h := make([]byte, headerSize)
	if _, err := r.ReadAt(h, 0); err != nil {
		return nil, fmt.Errorf("wim: header: %w", err)
	}
	if !bytes.Equal(h[:8], []byte("MSWIM\x00\x00\x00")) {
		return nil, errors.New("wim: not a Windows image file")
	}
	// The XML resource's header: a 7-byte compressed size and a flags byte,
	// its offset, and its size; the XML itself is never compressed.
	res := h[72:96]
	size := binary.LittleEndian.Uint64(res[16:24])
	offset := binary.LittleEndian.Uint64(res[8:16])
	if size == 0 || size > maxXML {
		return nil, fmt.Errorf("wim: an XML resource of %d bytes", size)
	}
	data := make([]byte, size)
	if _, err := r.ReadAt(data, int64(offset)); err != nil {
		return nil, fmt.Errorf("wim: XML resource: %w", err)
	}
	return parseXML(data)
}

// parseXML decodes the XML resource: UTF-16, little-endian, from a BOM.
func parseXML(data []byte) ([]Image, error) {
	if len(data) < 2 || len(data)%2 != 0 {
		return nil, errors.New("wim: the XML resource is not UTF-16")
	}
	u := make([]uint16, len(data)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(data[2*i:])
	}
	if u[0] == 0xFEFF {
		u = u[1:]
	}
	var doc struct {
		Images []struct {
			Index   string `xml:"INDEX,attr"`
			Name    string `xml:"NAME"`
			Windows struct {
				EditionID string `xml:"EDITIONID"`
			} `xml:"WINDOWS"`
		} `xml:"IMAGE"`
	}
	if err := xml.Unmarshal([]byte(string(utf16.Decode(u))), &doc); err != nil {
		return nil, fmt.Errorf("wim: XML resource: %w", err)
	}
	images := make([]Image, 0, len(doc.Images))
	for _, im := range doc.Images {
		index, err := strconv.Atoi(im.Index)
		if err != nil || index < 1 {
			return nil, fmt.Errorf("wim: an image with index %q", im.Index)
		}
		images = append(images, Image{Index: index, Name: strings.TrimSpace(im.Name), EditionID: strings.TrimSpace(im.Windows.EditionID)})
	}
	if len(images) == 0 {
		return nil, errors.New("wim: no images")
	}
	return images, nil
}
