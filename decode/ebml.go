// EBML primitives (RFC 8794) shared by the native Matroska/WebM readers.
//
// Purpose:
//   This file knows EBML and nothing about Matroska. It reads element IDs and
//   data sizes, classifies the two variable-integer flavours, and walks a
//   bounded tree of master elements. The WebM Opus reader is its only consumer
//   today; keeping the Matroska vocabulary out of here lets a future reader
//   reuse the same primitives.
//
// Key Components:
//   - ebmlID(): an Element ID VINT with the marker bit kept (RFC 8794 section 5)
//   - ebmlVint(): a Data Size VINT with the marker bit stripped (section 6)
//   - ebmlElement / ebmlChildren(): one element header and a bounded walk
//
// Spec notes:
//   - IDs are 1 to 4 bytes and sizes 1 to 8 bytes; WebM bounds both with
//     EBMLMaxIDLength and EBMLMaxSizeLength. The limits here are absolute
//     RFC 8794 limits, not the file's declared ones.
//   - A 1-byte ID 0xFF is reserved (RFC 8794 section 5 plus the Matroska
//     errata); every other 1-byte ID, including 0x80, is legal. That matters
//     because 0x80 appears inside compressed block data and a byte scan would
//     mistake it for an element.
//   - A size whose data bits are all ones means "unknown", which is legal only
//     for a master element and terminates at its parent's end (section 6.2).
//   - Void (0xEC) and CRC-32 (0xBF) may appear at any level and are skipped
//     rather than reported as errors (section 11.3).
//
// Ownership:
//   Every function here is pure: it reads from a byte slice the caller owns and
//   never seeks, allocates beyond the small returned headers, or keeps state.

package decode

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// ebmlMaxIDLen and ebmlMaxSizeLen are the RFC 8794 ceilings. WebM is stricter
// (4 and 8), which happens to be the same, so the format limit and the file's
// declared limit agree; the constants are named for the spec limit.
const (
	ebmlMaxIDLen   = 4
	ebmlMaxSizeLen = 8

	// ebmlMaxDepth bounds recursion so a corrupt file whose masters nest
	// without end cannot exhaust the stack. Matroska uses well under ten
	// levels; the walker stops before it trusts a deeper tree.
	ebmlMaxDepth = 8
)

// Element IDs this package's readers match on. Declared here because the
// generic walker needs them for Void and CRC-32; the Matroska vocabulary lives
// in webmopus.go.
const (
	ebmlIDVoid  = 0xEC
	ebmlIDCRC32 = 0xBF
)

// Errors from the EBML layer. They are wrapped by the reader with the element
// name it was expecting so a message identifies the field, not just the byte.
var (
	ErrEBMLBadVint   = errors.New("decode: invalid EBML variable-length integer")
	ErrEBMLTruncated = errors.New("decode: truncated EBML element")
)

// ebmlID reads the Element ID at off. The returned id keeps its marker bit, so
// element IDs are compared against their table values (RFC 8794 section 5):
// the readers' id* constants are those raw values.
//
// A leading zero byte, a run longer than ebmlMaxIDLen, or the reserved 1-byte
// 0xFF are rejected. RFC 8794 reserves 0xFF; Matroska's errata confirm every
// other 1-byte ID, including 0x80, is valid.
func ebmlID(b []byte, off int) (id uint32, width int, err error) {
	if off >= len(b) {
		return 0, 0, fmt.Errorf("%w: id past end at %d", ErrEBMLTruncated, off)
	}
	first := b[off]
	if first == 0 {
		return 0, 0, fmt.Errorf("%w: zero leading byte at %d", ErrEBMLBadVint, off)
	}

	width = 1
	mask := byte(0x80)
	for first&mask == 0 {
		mask >>= 1
		width++
		if width > ebmlMaxIDLen {
			return 0, 0, fmt.Errorf("%w: id longer than %d bytes at %d", ErrEBMLBadVint, ebmlMaxIDLen, off)
		}
	}
	// The all-ones 1-byte ID is the reserved value; Matroska does not use it.
	if width == 1 && first == 0xFF {
		return 0, 0, fmt.Errorf("%w: reserved id 0xFF at %d", ErrEBMLBadVint, off)
	}
	if off+width > len(b) {
		return 0, 0, fmt.Errorf("%w: id needs %d bytes at %d", ErrEBMLTruncated, width, off)
	}

	id = 0
	for i := range width {
		id = id<<8 | uint32(b[off+i])
	}

	return id, width, nil
}

// ebmlVint reads a Data Size VINT at off. The marker bit is stripped (RFC 8794
// section 6), so 0x85 decodes to 5. unknown is true when every data bit is set,
// which a master element uses to mean "size not known here".
func ebmlVint(b []byte, off int) (val uint64, width int, unknown bool, err error) {
	if off >= len(b) {
		return 0, 0, false, fmt.Errorf("%w: vint past end at %d", ErrEBMLTruncated, off)
	}
	first := b[off]
	if first == 0 {
		return 0, 0, false, fmt.Errorf("%w: zero leading byte at %d", ErrEBMLBadVint, off)
	}

	width = 1
	mask := byte(0x80)
	for first&mask == 0 {
		mask >>= 1
		width++
		if width > ebmlMaxSizeLen {
			return 0, 0, false, fmt.Errorf("%w: size longer than %d bytes at %d", ErrEBMLBadVint, ebmlMaxSizeLen, off)
		}
	}
	if off+width > len(b) {
		return 0, 0, false, fmt.Errorf("%w: size needs %d bytes at %d", ErrEBMLTruncated, width, off)
	}

	val = uint64(first & (mask - 1))
	for i := 1; i < width; i++ {
		val = val<<8 | uint64(b[off+i])
	}
	// All data bits set is the unknown-size sentinel. It cannot be a real size
	// because the all-ones encoding is reserved for exactly this.
	unknown = val == uint64(1)<<(7*width)-1

	return val, width, unknown, nil
}

// ebmlElement is one element whose header has been read. DataOff and Next are
// absolute offsets into the same buffer the header came from.
//
// Next is zero for an unknown-size element because its end is not encoded in
// its own header; a walker derives it from the parent's end instead.
type ebmlElement struct {
	ID      uint32
	Size    uint64
	Unknown bool
	DataOff int64
	Next    int64
}

// readEBMLHeader parses the element header at off and resolves Next for a
// known-size element. limit is the exclusive end of the enclosing context: a
// known size that runs past it is a truncation error, and the unknown case is
// left for the caller to terminate at limit.
func readEBMLHeader(b []byte, off int64) (ebmlElement, error) {
	id, idw, err := ebmlID(b, int(off))
	if err != nil {
		return ebmlElement{}, err
	}
	size, szw, unknown, err := ebmlVint(b, int(off)+idw)
	if err != nil {
		return ebmlElement{}, err
	}

	e := ebmlElement{
		ID:      id,
		Size:    size,
		Unknown: unknown,
		DataOff: off + int64(idw+szw),
	}
	if !unknown {
		e.Next = e.DataOff + int64(size)
	}

	return e, nil
}

// ebmlChildren walks the direct children of the master element starting at
// start and ending before end, calling fn for each. It handles the two global
// elements by skipping them (RFC 8794 section 11.3) and, for an unknown-size
// child, reports the element with Next set to the parent's end so fn sees a
// bounded extent.
//
// The walker is deliberately not recursive: nesting is expressed by fn calling
// ebmlChildren again on a child's own range, which keeps the depth bound in one
// place. A malformed child that overruns end stops the walk with an error
// rather than reading past the parent.
func ebmlChildren(b []byte, start, end int64, fn func(ebmlElement) error) error {
	if start > end || end > int64(len(b)) {
		return fmt.Errorf("%w: child range [%d, %d) outside %d bytes", ErrEBMLTruncated, start, end, len(b))
	}

	off := start
	for off < end {
		e, err := readEBMLHeader(b, off)
		if err != nil {
			return err
		}
		if e.Unknown {
			// An unknown-size child runs to the parent's end, so it is the
			// last child the walker can see.
			e.Next = end
		} else if e.Next > end {
			return fmt.Errorf("%w: element %#x ends at %d past %d", ErrEBMLTruncated, e.ID, e.Next, end)
		}

		if e.ID != ebmlIDVoid && e.ID != ebmlIDCRC32 {
			if err := fn(e); err != nil {
				return err
			}
		}

		if e.Next <= off {
			// A zero-length element must still advance the cursor; anything
			// that fails to is a loop, not data.
			return fmt.Errorf("%w: element %#x at %d did not advance", ErrEBMLBadVint, e.ID, off)
		}
		off = e.Next
	}

	return nil
}

// ebmlUint decodes an unsigned integer element (RFC 8794 section 7.1): a
// big-endian integer of 0 to 8 bytes, where 0 bytes is the value zero.
func ebmlUint(b []byte, off, size int64) (uint64, error) {
	if size < 0 || size > 8 {
		return 0, fmt.Errorf("%w: uinteger size %d", ErrEBMLBadVint, size)
	}
	if off < 0 || off+size > int64(len(b)) {
		return 0, fmt.Errorf("%w: uinteger at %d size %d", ErrEBMLTruncated, off, size)
	}

	var v uint64
	for i := int64(0); i < size; i++ {
		v = v<<8 | uint64(b[off+i])
	}

	return v, nil
}

// ebmlFloat decodes a float element (RFC 8794 section 7.2): 4 bytes are an
// IEEE 754 single, 8 are a double, both big-endian. Any other width is
// malformed; Duration in a WebM Info element is the reason this exists and why
// reading it as an integer yields garbage.
func ebmlFloat(b []byte, off, size int64) (float64, error) {
	if off < 0 || off+size > int64(len(b)) {
		return 0, fmt.Errorf("%w: float at %d size %d", ErrEBMLTruncated, off, size)
	}
	switch size {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b[off : off+4]))), nil
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(b[off : off+8])), nil
	default:
		return 0, fmt.Errorf("%w: float size %d", ErrEBMLBadVint, size)
	}
}

// readFullAt reads size bytes at off into a fresh slice, bounded by limit: the
// exclusive end of the extent the caller knows is readable. It gives the
// walkers a single place to turn a short read or an oversized element into
// ErrEBMLTruncated instead of leaking io.ErrUnexpectedEOF, and it is the one
// guard that stops a corrupt size VINT from reaching make() and panicking.
func readFullAt(r io.ReaderAt, off int64, size int64, limit int64) ([]byte, error) {
	if size < 0 || off < 0 {
		return nil, fmt.Errorf("%w: negative read off %d size %d", ErrEBMLTruncated, off, size)
	}
	if limit < off || off+size > limit {
		return nil, fmt.Errorf("%w: read of %d bytes at %d exceeds limit %d", ErrEBMLTruncated, size, off, limit)
	}
	buf := make([]byte, size)
	if _, err := r.ReadAt(buf, off); err != nil {
		return nil, fmt.Errorf("%w: read %d bytes at %d: %w", ErrEBMLTruncated, size, off, err)
	}

	return buf, nil
}
