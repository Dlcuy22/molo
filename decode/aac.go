// AAC-LC decoding through github.com/tphakala/go-aac. This is the lossy path:
// it needs no runtime shared library and no cgo, and it decodes the ADTS frame
// stream (.aac) rather than the raw AAC inside an MP4 container, which is a
// different demuxer's job.
//
// ADTS is a bare frame stream. Each frame carries its own 7-byte header with
// the sample rate and channel configuration, but there is no field for the
// total length and no frame index. The header does carry the frame length,
// though, so a single pass over the 7-byte headers builds a byte-offset index
// without decoding a sample. That index gives both an exact Probe total and a
// native seek, the same approach ffmpeg's ADTS probe takes. go-aac itself is
// asked for one access unit at a time (pcm.FrameDecoder), so this adapter
// demuxes the framing rather than the library parsing it again.
//
// The decoder normalizes whatever the file holds (44.1 or 48 kHz, mono or
// stereo) to the engine's canonical 48000 Hz stereo float32, the same contract
// the other byte-oriented decoders satisfy. The shared pcmConverter does the
// rate, channel and width conversion; go-aac emits interleaved little-endian
// S16, so the stored width is always two bytes.
//
// Dependencies: github.com/tphakala/go-aac.

package decode

import (
	"fmt"
	"io"
	"os"

	"github.com/dlcuy22/molo/core"
	"github.com/tphakala/go-aac/pcm"
)

// init registers the AAC decoder.
func init() {
	Register(NewAacFactory())
}

// AacFactory decodes AAC-LC in ADTS without cgo.
type AacFactory struct{}

// NewAacFactory returns a factory for the AAC-LC in ADTS decoder.
func NewAacFactory() *AacFactory { return &AacFactory{} }

func (f *AacFactory) Name() string { return "aac" }

// FriendlyName is the label a UI shows for this codec. It repeats the family
// name deliberately: the picker renders "{family} {friendly}" but suppresses
// the repetition, so "Aac" reads as one word rather than "Aac Aac". Unlike
// Opus, where the label names a property ("Portable"), there is no second AAC
// variant to distinguish, so no qualifier is added.
func (f *AacFactory) FriendlyName() string { return "Aac" }

// Weight only breaks ties between codecs that claim the same extension. No other
// factory claims .aac, so this value never competes.
func (f *AacFactory) Weight() int { return 90 }

func (f *AacFactory) Exts() []string { return []string{".aac"} }

// Match claims an ADTS syncword. The 12-bit sync is 0xFFF, so the first byte
// must be 0xFF and the second byte's high nibble must be 0xF. The mask 0xF6
// additionally requires the two MPEG layer bits (byte 1, bits 2 and 1) to be
// zero, which ADTS mandates; it deliberately ignores the MPEG version bit and
// the protection bit, which legitimately vary between valid streams.
//
// A match needs two bytes, so a lone 0xFF is never claimed, and MPEG audio
// frames set the layer bits non-zero: 0xFF 0xFB (Layer III) and its siblings
// fall out of the mask and are left to the codec that owns them.
func (f *AacFactory) Match(magic []byte) bool {
	return len(magic) >= 2 && magic[0] == 0xFF && magic[1]&0xF6 == 0xF0
}

// adtsIndexEntry maps one ADTS frame to its byte offset and first sample.
type adtsIndexEntry struct {
	offset      int64
	startSample int64
}

// adtsSampleRates is the ADTS sampling_frequency_index table, in index order.
// Indices 13..15 are reserved or forbidden, so they hold zero; a stream that
// declares one is rejected by the caller's rate check rather than read past the
// table. The slice is exactly 16 entries so a 4-bit index can never be out of
// range.
var adtsSampleRates = [16]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000,
	22050, 16000, 12000, 11025, 8000, 7350,
	0, 0, 0,
}

// buildADTSIndex walks the 7-byte ADTS headers and returns one entry per frame
// plus the sample rate parsed from the first header. It skips a leading ID3v2
// tag if present and requires the first byte after it to be an ADTS sync. A
// frame that declares a length below its header or that runs past EOF ends the
// scan without failing, so a trailing tag or a truncated final frame keeps the
// entries gathered so far. The file is left at offset 0 on return.
func buildADTSIndex(f *os.File, size int64) ([]adtsIndexEntry, int, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}

	off, err := adtsAudioStart(f, size)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _, _ = f.Seek(0, io.SeekStart) }()

	var (
		entries []adtsIndexEntry
		rate    int
		hdr     [7]byte
	)

	for off >= 0 && off+7 <= size {
		if _, err := f.ReadAt(hdr[:], off); err != nil {
			break
		}
		if hdr[0] != 0xFF || hdr[1]&0xF6 != 0xF0 {
			break
		}
		frameLen := int64(hdr[3]&0x03)<<11 | int64(hdr[4])<<3 | int64(hdr[5])>>5
		if frameLen < 7 || off+frameLen > size {
			break
		}
		if rate == 0 {
			rate = adtsSampleRates[(hdr[2]>>2)&0x0F]
		}
		entries = append(entries, adtsIndexEntry{
			offset:      off,
			startSample: int64(len(entries)) * 1024,
		})
		// frameLen >= 7, so off always advances and the scan terminates.
		off += frameLen
	}

	return entries, rate, nil
}

// adtsHeaderShape reads the first frame's header and returns its sample rate and
// channel configuration. It wires the cheap path Probe takes for DurationUnknown
// and the shape check Open does before indexing.
func adtsHeaderShape(f *os.File, size int64) (int, int, error) {
	off, err := adtsAudioStart(f, size)
	if err != nil {
		return 0, 0, err
	}
	if off < 0 {
		return 0, 0, fmt.Errorf("decode: no ADTS frame in the first bytes")
	}

	var hdr [7]byte
	if _, err := f.ReadAt(hdr[:], off); err != nil {
		return 0, 0, err
	}
	rate := adtsSampleRates[(hdr[2]>>2)&0x0F]
	chans := int(hdr[2]&0x01)<<2 | int(hdr[3]>>6)

	return rate, chans, nil
}

// adtsAudioStart returns the byte offset of the first audio frame, skipping an
// ID3v2 tag when the file begins with one. It requires an ADTS sync there: a
// file without one has no audio this decoder can index.
func adtsAudioStart(f *os.File, size int64) (int64, error) {
	var head [10]byte
	n, err := f.ReadAt(head[:], 0)
	if err != nil && err != io.EOF {
		return 0, err
	}
	if n < 10 {
		return -1, nil
	}

	off := int64(0)
	if head[0] == 'I' && head[1] == 'D' && head[2] == '3' {
		// The tag size is a 4-byte synchsafe integer; the 10-byte header sits
		// in front of it.
		tagSize := int64(head[6]&0x7F)<<21 | int64(head[7]&0x7F)<<14 |
			int64(head[8]&0x7F)<<7 | int64(head[9]&0x7F)
		off = 10 + tagSize
	}
	if off+7 > size {
		return -1, nil
	}
	if _, err := f.ReadAt(head[:7], off); err != nil {
		return -1, nil
	}
	if head[0] != 0xFF || head[1]&0xF6 != 0xF0 {
		return -1, nil
	}

	return off, nil
}

func (f *AacFactory) Open(path string) (Decoder, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	fi, err := file.Stat()
	if err != nil {
		file.Close()

		return nil, fmt.Errorf("decode: open AAC %s: %w", path, err)
	}

	rate, chans, err := adtsHeaderShape(file, fi.Size())
	if err != nil {
		file.Close()

		return nil, fmt.Errorf("decode: open AAC %s: %w", path, err)
	}
	if rate < 1 || chans < 1 || chans > 2 {
		file.Close()

		return nil, fmt.Errorf("decode: AAC reports an unusable stream %d Hz %d ch", rate, chans)
	}

	index, _, err := buildADTSIndex(file, fi.Size())
	if err != nil {
		file.Close()

		return nil, fmt.Errorf("decode: open AAC %s: %w", path, err)
	}
	if len(index) == 0 {
		file.Close()

		return nil, fmt.Errorf("decode: AAC %s carries no usable ADTS frame", path)
	}

	d, err := newAacDecoder(file, index, rate, chans)
	if err != nil {
		file.Close()

		return nil, err
	}

	return d, nil
}

// Probe reports the stream shape. DurationUnknown reads only the first header,
// so it stays as cheap as an open. DurationProbe scans the 7-byte headers the
// decoder indexes, which yields the exact frame count without decoding a
// sample; it is still far cheaper than draining the file.
func (f *AacFactory) Probe(path string, opts ProbeOptions) (core.StreamInfo, error) {
	info := core.StreamInfo{Format: core.CanonicalFormat, TotalFrames: -1}

	file, err := os.Open(path)
	if err != nil {
		return info, err
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		return info, err
	}

	rate, chans, err := adtsHeaderShape(file, fi.Size())
	if err != nil {
		return info, err
	}
	if rate < 1 || chans < 1 || chans > 2 {
		return info, fmt.Errorf("decode: AAC reports an unusable stream %d Hz %d ch", rate, chans)
	}

	if opts.Duration == core.DurationUnknown {
		return info, nil
	}

	index, indexRate, err := buildADTSIndex(file, fi.Size())
	if err != nil {
		return info, err
	}
	if len(index) == 0 || indexRate < 1 {
		return info, fmt.Errorf("decode: AAC reports an unusable stream %d Hz", indexRate)
	}

	total := int64(len(index)) * 1024
	info.SourceSamples = total
	info.SourceRate = indexRate
	if frames, ok := canonicalFrames(uint64(total), indexRate); ok {
		info.TotalFrames = frames
	}

	return info, nil
}

// aacDecoder adapts go-aac's frame decoder to the engine's ReadFrames contract,
// normalizing rate and channels through the shared pcmConverter. It demuxes
// ADTS itself from the byte index built at open, so go-aac is handed whole
// access units and never re-parses the framing. go-aac emits interleaved
// little-endian S16, so the stored width passed to the converter is a constant.
type aacDecoder struct {
	file *os.File
	dec  *pcm.FrameDecoder

	conv *pcmConverter

	// index maps every ADTS frame to its offset and first source sample.
	index []adtsIndexEntry
	// auIndex is the next index entry to read.
	auIndex int
	// total is the playable frame count in the canonical domain, or -1.
	total int64
	// srcSamples and srcRate are the exact source-domain length.
	srcSamples int64
	srcRate    int

	// au receives one access unit (header plus payload) read from the file.
	au []byte
	// s16 receives S16 bytes from go-aac, grown in place across frames.
	s16 []byte
	// raw is the buffer pcmFillLoop hands to readPCM.
	raw []byte
	// out holds one block of interleaved stereo float32, which pending points
	// into until the caller has taken it.
	out     []float32
	pending []float32

	// eof records that the source signalled end, so one final drain can emit
	// the interpolation tail.
	eof bool

	pos    int64
	closed bool
}

func newAacDecoder(file *os.File, index []adtsIndexEntry, rate, chans int) (*aacDecoder, error) {
	if rate < 1 || chans < 1 || chans > 2 {
		return nil, fmt.Errorf("decode: AAC reports an unusable stream %d Hz %d ch", rate, chans)
	}

	const bytesPS = 2 // go-aac always hands back S16

	total := int64(-1)
	srcSamples := int64(len(index)) * 1024
	if frames, ok := canonicalFrames(uint64(srcSamples), rate); ok {
		total = frames
	}

	d := &aacDecoder{
		file:       file,
		dec:        pcm.NewADTSDecoder(),
		conv:       newPCMConverter(rate, chans, bytesPS),
		index:      index,
		total:      total,
		srcSamples: srcSamples,
		srcRate:    rate,
	}
	d.raw = rawFrames()
	// au holds one ADTS frame: a 7-byte header plus payload, at most 8191
	// bytes by the 13-bit length field.
	d.au = make([]byte, 8191)
	// out is one block of canonical stereo; pending aliases it, so it must not
	// be resized while a caller still holds frames from it.
	d.out = outBlock()

	return d, nil
}

func (d *aacDecoder) Info() core.StreamInfo {
	return core.StreamInfo{
		Format:        core.CanonicalFormat,
		TotalFrames:   d.total,
		SourceSamples: d.srcSamples,
		SourceRate:    d.srcRate,
	}
}

// DecoderName names the codec implementation behind this decoder.
func (d *aacDecoder) DecoderName() string { return "go-aac" }

// ParserName names the container handling. The ADTS framing is this adapter's;
// the parenthetical keeps it distinct from the raw-access-unit path the same
// library offers for MP4.
func (d *aacDecoder) ParserName() string { return "go-aac (adts)" }

func (d *aacDecoder) ReadFrames(dst []float32) (int, error) {
	return readCanonical(dst, d.out, &d.pending, d.fill, &d.pos, d.total, d.closed)
}

// fill decodes and normalizes until one block of stereo output is ready.
func (d *aacDecoder) fill() error {
	return pcmFillLoop(d.conv, d.out, &d.pending, &d.eof, d.readPCM, d.raw, d.conv.feed)
}

// readPCM reads the next ADTS access unit from the file and decodes it into
// dst, in place of go-aac's own reader.
func (d *aacDecoder) readPCM(dst []byte) (int, error) {
	if d.auIndex >= len(d.index) {
		return 0, io.EOF
	}

	au := d.readAU(d.auIndex)
	out, samples, err := d.dec.DecodeFrame(d.s16[:0], au)
	if err != nil {
		return 0, fmt.Errorf("decode: decode AAC frame %d: %w", d.auIndex, err)
	}
	d.s16 = out
	d.auIndex++

	if samples == 0 {
		return 0, nil
	}

	// One AAC-LC access unit is at most a 13-bit frame length of interleaved
	// S16, far under the raw buffer, so this copy is always whole.
	return copy(dst, d.s16), nil
}

// readAU returns one whole ADTS access unit (7-byte header plus payload) from
// the index entry at i. It bounds the unit by the next entry's offset or, for
// the last frame, by its own declared length.
func (d *aacDecoder) readAU(i int) []byte {
	entry := d.index[i]

	var frameLen int
	if i+1 < len(d.index) {
		frameLen = int(d.index[i+1].offset - entry.offset)
	} else {
		// The index has no following entry to bound the last frame, so read
		// its own header for the declared length.
		var hdr [7]byte
		if _, err := d.file.ReadAt(hdr[:], entry.offset); err == nil {
			frameLen = int(hdr[3]&0x03)<<11 | int(hdr[4])<<3 | int(hdr[5])>>5
		}
	}
	if frameLen < 7 || frameLen > len(d.au) {
		frameLen = len(d.au)
	}
	au := d.au[:frameLen]
	// A short read leaves the tail stale; the frame length bounds what the
	// decoder consumes, and a well-formed file always yields the whole unit.
	_, _ = d.file.ReadAt(au, entry.offset)

	return au
}

// SeekFrame repositions so the next delivered frame is frame. AAC-LC frames
// cannot start mid-frame, so a canonical target is converted to a source sample,
// floored to the frame index at or before it, and the byte index is binary
// searched for that frame's offset. pos is set from the landing frame, so it is
// authoritative even when the target sat inside a 1024-sample frame.
func (d *aacDecoder) SeekFrame(frame int64) error {
	if d.closed {
		return ErrClosed
	}
	if frame < 0 {
		return fmt.Errorf("decode: negative seek target %d", frame)
	}

	srcSample := canonicalSamples(frame, d.conv.srcRate)

	// Largest entry with startSample <= srcSample, or entry 0 when the target
	// precedes the first frame.
	lo, hi := 0, len(d.index)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if d.index[mid].startSample <= srcSample {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	entry := d.index[lo]

	// readAU reads by explicit offset, but position the file too so it is left
	// where the stream resumes, matching the MP3 path.
	if _, err := d.file.Seek(entry.offset, io.SeekStart); err != nil {
		return fmt.Errorf("decode: seek to frame %d: %w", frame, err)
	}

	if err := d.dec.Reset(); err != nil {
		return fmt.Errorf("decode: seek to frame %d: %w", frame, err)
	}
	// AAC-LC reconstructs each frame by overlap-add with the previous frame, so
	// the decoder needs the frame before the landing one for its history. Decode
	// it into scratch and discard the output; without this the first landing
	// frame is windowed against silence and the seek would not match a straight
	// decode. This mirrors the priming go-mp3 does inside SeekToSample.
	if lo > 0 {
		_, _, err := d.dec.DecodeFrame(d.s16[:0], d.readAU(lo-1))
		if err != nil {
			return fmt.Errorf("decode: seek to frame %d: %w", frame, err)
		}
	}
	d.auIndex = lo

	// Anything the resampler still held belongs to the abandoned position.
	d.conv.reset()
	d.pending = nil
	d.eof = false
	d.pos = canonicalFramesOf(entry.startSample, d.conv)

	return nil
}

func (d *aacDecoder) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	d.pending = nil
	d.conv.src = nil
	// go-aac's FrameDecoder holds no OS resource of its own, but honoring an
	// io.Closer keeps this adapter correct if the library ever grows one.
	if c, ok := any(d.dec).(io.Closer); ok {
		_ = c.Close()
	}
	d.dec = nil
	if d.file != nil {
		d.file.Close()
		d.file = nil
	}

	return nil
}
