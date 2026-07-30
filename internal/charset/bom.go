package charset

import "bytes"

// Byte order marks, longest first. The order matters: the UTF-32LE BOM starts
// with the UTF-16LE BOM, so testing FF FE first would misread every UTF-32LE
// file as UTF-16LE.
var boms = []struct {
	bytes []byte
	name  string
}{
	{[]byte{0x00, 0x00, 0xFE, 0xFF}, nameUTF32BE},
	{[]byte{0xFF, 0xFE, 0x00, 0x00}, nameUTF32LE},
	{[]byte{0xEF, 0xBB, 0xBF}, nameUTF8},
	{[]byte{0xFE, 0xFF}, nameUTF16BE},
	{[]byte{0xFF, 0xFE}, nameUTF16LE},
}

// detectBOM returns the byte order mark at the start of b and the canonical
// name of the encoding it announces. It returns (nil, "") when there is none.
//
// The returned slice aliases b, so a caller that keeps it past the lifetime of
// b must copy it.
func detectBOM(b []byte) ([]byte, string) {
	for _, bom := range boms {
		if bytes.HasPrefix(b, bom.bytes) {
			return b[:len(bom.bytes)], bom.name
		}
	}
	return nil, ""
}

// BOMFor returns the byte order mark for the given canonical encoding name, or
// nil if that encoding has no BOM.
func BOMFor(name string) []byte {
	for _, bom := range boms {
		if bom.name == name {
			return bytes.Clone(bom.bytes)
		}
	}
	return nil
}
