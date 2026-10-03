package decode

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/dlcuy22/molo/core"
)

// oggTailWindow is how far back from EOF a duration probe looks for the final
// page header. One Opus page holds at most 255 lacing values, so a page is
// under 65 KiB for any sane bitrate.
const oggTailWindow = 64 * 1024

const (
	oggCapture     = "OggS"
	oggHeaderLen   = 27
	oggSegmentOff  = 26
	oggGranuleOff  = 6
	oggGranuleSize = 8
)

var errNoOggPage = errors.New("decode: no complete Ogg page found at end of file")

// oggGranule is the granule position of a file's final page.
type oggGranule struct {
	granule uint64
}

// probeOggOpus reports the stream shape of an Ogg Opus file. DurationUnknown
// reads only the head; DurationProbe reads the last window and derives the
// frame count from the final granule position. It never decodes audio.
func probeOggOpus(path string, mode core.DurationMode, window int64) (core.StreamInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return core.StreamInfo{}, err
	}
	defer f.Close()

	head, err := readOggOpusHead(f)
	if err != nil {
		return core.StreamInfo{}, err
	}

	info := core.StreamInfo{
		// Format describes the decoded stream, and the engine's currency is
		// always 48 kHz stereo float32. Reporting the source's channel count
		// here would contradict what Open returns for the same file.
		Format: core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32},
		// Unknown until the tail is read, even when the caller asked for a probe
		// that then fails: a failed probe must not look like a zero-length file.
		TotalFrames: -1,
	}
	if mode == core.DurationUnknown {
		return info, nil
	}

	last, err := lastOggPage(f, window)
	if err != nil {
		if !errors.Is(err, errNoOggPage) {
			return info, err
		}
		if mode != core.DurationScan {
			// The final page is outside the read window or the tail is damaged.
			// Reporting "unknown" is honest; guessing would corrupt seek and
			// duration downstream.
			return info, nil
		}
		// DurationScan explicitly pays for a full pass, so walk the pages
		// forward instead of trusting the tail.
		frames, scanErr := scanOggOpusGranule(f)
		if scanErr != nil {
			return info, nil
		}
		// Same guard as the tail path: a scan that finds granule < preSkip is
		// corrupt data, not a negative frame count. Both paths report it the
		// same way so callers never see a path-dependent result.
		if frames < int64(head.preSkip) {
			return info, fmt.Errorf("decode: granule %d precedes pre-skip %d", frames, head.preSkip)
		}
		info.TotalFrames = frames - int64(head.preSkip)

		return info, nil
	}

	// RFC 7845: granule counts decoded samples at 48 kHz and includes pre-skip.
	frames := int64(last.granule) - int64(head.preSkip)
	if frames < 0 {
		return info, fmt.Errorf("decode: granule %d precedes pre-skip %d", last.granule, head.preSkip)
	}
	info.TotalFrames = frames

	return info, nil
}

type oggOpusHead struct {
	preSkip uint16
}

// readOggOpusHead reads exactly the first page and validates the OpusHead
// packet in it. The head is never assumed to be identically sized across files.
func readOggOpusHead(f *os.File) (oggOpusHead, error) {
	header := make([]byte, oggHeaderLen)
	if _, err := f.ReadAt(header, 0); err != nil {
		return oggOpusHead{}, fmt.Errorf("decode: read Ogg header: %w", err)
	}
	if string(header[:4]) != oggCapture {
		return oggOpusHead{}, fmt.Errorf("decode: %q is not an Ogg stream", f.Name())
	}

	segments := int(header[oggSegmentOff])
	lacing := make([]byte, segments)
	if _, err := f.ReadAt(lacing, oggHeaderLen); err != nil {
		return oggOpusHead{}, fmt.Errorf("decode: read Ogg lacing: %w", err)
	}

	firstPacket := 0
	for _, size := range lacing {
		firstPacket += int(size)
		if size < 255 {
			break
		}
	}

	payload := int64(oggHeaderLen + segments)
	head := make([]byte, firstPacket)
	if _, err := f.ReadAt(head, payload); err != nil {
		return oggOpusHead{}, fmt.Errorf("decode: read OpusHead: %w", err)
	}
	if len(head) < 19 || string(head[:8]) != "OpusHead" {
		return oggOpusHead{}, fmt.Errorf("decode: first packet is not OpusHead")
	}

	return oggOpusHead{
		preSkip: binary.LittleEndian.Uint16(head[10:12]),
	}, nil
}

// lastOggPage reads at most window bytes from the end of f and returns the
// page that ends exactly at EOF. A file whose final page header starts before
// the window yields errNoOggPage rather than a guessed granule.
func lastOggPage(f *os.File, window int64) (oggGranule, error) {
	size, err := f.Seek(0, 2)
	if err != nil {
		return oggGranule{}, err
	}
	if size < oggHeaderLen {
		return oggGranule{}, errNoOggPage
	}

	if window < oggHeaderLen {
		window = oggHeaderLen
	}
	start := size - window
	if start < 0 {
		start = 0
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return oggGranule{}, err
	}

	for i := len(buf) - oggHeaderLen; i >= 0; i-- {
		if string(buf[i:i+4]) != oggCapture {
			continue
		}
		pageEnd, err := oggPageEnd(buf[i:], start+int64(i))
		if err != nil {
			continue
		}
		if pageEnd != size {
			continue
		}

		granule := binary.LittleEndian.Uint64(buf[i+oggGranuleOff : i+oggGranuleOff+oggGranuleSize])
		// The all-ones granule means no packet completed on the page, so it
		// cannot be the end of a decodable stream. Rejecting it costs nothing
		// and filters out an accidental OggS match inside compressed data.
		if granule == math.MaxUint64 {
			continue
		}

		return oggGranule{granule: granule}, nil
	}

	return oggGranule{}, errNoOggPage
}

// oggPageEnd returns the byte offset just past a page starting at buf[0], or
// an error when the buffer does not hold the whole page.
func oggPageEnd(buf []byte, headerOffset int64) (int64, error) {
	if len(buf) < oggHeaderLen {
		return 0, errNoOggPage
	}
	segments := int(buf[oggSegmentOff])
	if len(buf) < oggHeaderLen+segments {
		return 0, errNoOggPage
	}
	body := 0
	for _, size := range buf[oggHeaderLen : oggHeaderLen+segments] {
		body += int(size)
	}

	end := headerOffset + int64(oggHeaderLen+segments+body)
	if end > headerOffset+int64(len(buf)) {
		return 0, errNoOggPage
	}

	return end, nil
}

// scanOggOpusGranule walks every page forward and returns the granule of the
// last audio page. It is the DurationScan fallback for files whose tail is
// unreadable; it still never decodes audio, but it does read the whole file.
func scanOggOpusGranule(f *os.File) (int64, error) {
	size, err := f.Seek(0, 2)
	if err != nil {
		return 0, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return 0, err
	}

	reader := bufio.NewReaderSize(f, 64*1024)
	var (
		offset  int64
		granule int64 = -1
		header  [oggHeaderLen]byte
	)
	for offset < size {
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			break
		}
		if string(header[:4]) != oggCapture {
			break
		}
		pageGranule := int64(binary.LittleEndian.Uint64(header[oggGranuleOff : oggGranuleOff+oggGranuleSize]))
		// -1 marks a page with no completed packet, which carries no position.
		if pageGranule >= 0 {
			granule = pageGranule
		}

		segments := int(header[oggSegmentOff])
		lacing := make([]byte, segments)
		if _, err := io.ReadFull(reader, lacing); err != nil {
			break
		}
		body := 0
		for _, l := range lacing {
			body += int(l)
		}
		if _, err := io.CopyN(io.Discard, reader, int64(body)); err != nil {
			break
		}
		offset += int64(oggHeaderLen + segments + body)
	}
	if granule < 0 {
		return 0, errNoOggPage
	}

	// Trailing bytes that do not form a page are ignored: the last granule we
	// did read is still the stream's true end. Only a stream with no readable
	// granule at all is an error.
	return granule, nil
}
