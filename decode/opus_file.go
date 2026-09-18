// Opus decoding through the system libopusfile via purego. This path needs no
// cgo, but it does require the shared library at runtime and can only open a
// filesystem path.
//
// Compared with the pure-Go decoder this one knows its total frame count at
// open and seeks natively, at the cost of a runtime dependency. Pre-skip and
// output gain are applied inside libopusfile and must not be re-applied here.
//
// Dependencies: github.com/ebitengine/purego, libopusfile.so.0 on Linux.

//go:build darwin || linux || netbsd

package decode

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"

	"github.com/dlcuy22/player/core"
	"github.com/ebitengine/purego"
)

// libopusfileName resolves the shared library to load. PLAYER_LIBOPUSFILE can
// override it, which lets a host point at a self-built library and lets the
// test suite prove the missing-library path is graceful.
func libopusfileName() string {
	if name := os.Getenv("PLAYER_LIBOPUSFILE"); name != "" {
		return name
	}

	return defaultLibopusfileName()
}

// Only the platforms the build tag admits reach this. Windows uses
// opus_file_stub.go, so there is deliberately no windows case here.
func defaultLibopusfileName() string {
	switch runtime.GOOS {
	case "darwin":
		return "libopusfile.0.dylib"
	default:
		return "libopusfile.so.0"
	}
}

// opusfile is the resolved set of libopusfile entry points.
type opusfile struct {
	open       func(path string, errPtr *int32) uintptr
	readFloat  func(of uintptr, pcm *float32, bufSize int32) int32
	pcmTotal   func(of uintptr, li int32) int64
	channelCnt func(of uintptr, li int32) int32
	free       func(of uintptr)
	pcmSeek    func(of uintptr, pcmOffset int64) int32
	pcmTell    func(of uintptr) int64
}

var (
	libMu     sync.Mutex
	libCache  = map[string]*opusfile{}
	libErrors = map[string]error{}
)

// resolveLibopusfile loads and caches the shared library, reporting why it
// could not be loaded so callers can choose between an error and a skipped
// test. Results are cached per library name because resolution is expensive and
// the name can vary between hosts.
func resolveLibopusfile() (*opusfile, error) {
	name := libopusfileName()

	libMu.Lock()
	defer libMu.Unlock()

	if lib, ok := libCache[name]; ok {
		return lib, nil
	}
	if err, ok := libErrors[name]; ok {
		return nil, err
	}

	handle, err := purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		err = fmt.Errorf("decode: load %s: %w", name, err)
		libErrors[name] = err

		return nil, err
	}

	var lib opusfile
	// Dlsym is used instead of RegisterLibFunc so a partially available library
	// reports an error rather than panicking deep inside purego.
	symbols := []struct {
		dst  any
		name string
	}{
		{&lib.open, "op_open_file"},
		{&lib.readFloat, "op_read_float_stereo"},
		{&lib.pcmTotal, "op_pcm_total"},
		{&lib.channelCnt, "op_channel_count"},
		{&lib.free, "op_free"},
		{&lib.pcmSeek, "op_pcm_seek"},
		{&lib.pcmTell, "op_pcm_tell"},
	}
	for _, sym := range symbols {
		addr, err := purego.Dlsym(handle, sym.name)
		if err != nil {
			err = fmt.Errorf("decode: resolve %s: %w", sym.name, err)
			libErrors[name] = err

			return nil, err
		}
		purego.RegisterFunc(sym.dst, addr)
	}

	libCache[name] = &lib

	return &lib, nil
}

// libopusfile returns the loaded library or the reason it is unavailable.
func loadLibopusfile() error {
	_, err := resolveLibopusfile()

	return err
}

// libopusfile error codes from opusfile.h. The exact number matters because
// OP_HOLE is recoverable while the rest are fatal for our purposes.
const (
	opHole = -3
)

// init registers the native decoder. Its weight is lower than the pure-Go
// factory, so it is never the automatic default; a caller that prefers native
// decoding asks for it by name with OpenNamed.
func init() {
	Register(NewLibopusfileFactory())
}

// LibopusfileFactory decodes Ogg Opus through the native library.
type LibopusfileFactory struct{}

func NewLibopusfileFactory() *LibopusfileFactory { return &LibopusfileFactory{} }

func (f *LibopusfileFactory) Name() string { return "opus-libopusfile" }

// FriendlyName is the label a UI shows for this codec.
func (f *LibopusfileFactory) FriendlyName() string { return "Fastest" }

// Weight leaves this below the pure-Go factory as the automatic default; the
// native library is faster but requires a runtime dependency.
func (f *LibopusfileFactory) Weight() int { return 80 }

func (f *LibopusfileFactory) Exts() []string { return []string{".opus", ".ogg"} }

// Match claims any Ogg stream, the same way the pure-Go factory does. Content
// sniffing cannot distinguish Opus from Vorbis without reading pages, so Open
// is what actually validates the payload.
func (f *LibopusfileFactory) Match(magic []byte) bool {
	return len(magic) >= 4 && string(magic[:4]) == oggCapture
}

func (f *LibopusfileFactory) Open(path string) (Decoder, error) {
	lib, err := resolveLibopusfile()
	if err != nil {
		return nil, err
	}

	of, err := lib.openFile(path)
	if err != nil {
		return nil, err
	}

	// A negative total means the length could not be established; such a file is
	// still decodable, so report the same sentinel the pure-Go path uses.
	total := max(lib.pcmTotal(of, -1), -1)

	// op_channel_count also guards against a header the library accepted but
	// that declares no channels, which would produce silent output forever.
	if ch := lib.channelCnt(of, -1); ch < 1 {
		lib.free(of)

		return nil, fmt.Errorf("decode: %s reports %d channels", path, ch)
	}

	return &libopusfileDecoder{
		lib:   lib,
		of:    of,
		total: total,
		buf:   make([]float32, maxOpusPacketSamples),
	}, nil
}

// Probe opens the file just long enough to read its frame total. DurationUnknown
// short-circuits before touching the library so the cheapest mode stays cheap.
func (f *LibopusfileFactory) Probe(path string, opts ProbeOptions) (core.StreamInfo, error) {
	info := core.StreamInfo{
		Format:      core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32},
		TotalFrames: -1,
	}
	if opts.Duration == core.DurationUnknown {
		return info, nil
	}
	lib, err := resolveLibopusfile()
	if err != nil {
		return info, err
	}

	of, err := lib.openFile(path)
	if err != nil {
		return info, err
	}
	defer lib.free(of)

	total := lib.pcmTotal(of, -1)
	if total < 0 {
		return info, fmt.Errorf("decode: %s has no known frame total", path)
	}
	info.TotalFrames = total

	return info, nil
}

// openFile wraps op_open_file, translating the C error code into a Go error.
func (l *opusfile) openFile(path string) (uintptr, error) {
	var code int32
	of := l.open(path, &code)
	if of == 0 {
		return 0, fmt.Errorf("decode: op_open_file(%s): code %d", path, code)
	}

	return of, nil
}

type libopusfileDecoder struct {
	lib   *opusfile
	of    uintptr
	total int64

	pending []float32
	buf     []float32

	pos    int64
	closed bool
}

func (d *libopusfileDecoder) Info() core.StreamInfo {
	return core.StreamInfo{
		Format: core.FrameFormat{Rate: 48000, Ch: 2, Fmt: core.F32},
		// op_read_float_stereo always yields two channels, but the source may be
		// mono; the output contract is stereo regardless.
		TotalFrames: d.total,
	}
}

// DecoderName names the native Opus implementation behind this decoder.
func (d *libopusfileDecoder) DecoderName() string { return "libopusfile" }

// ParserName names the container handling this decoder uses. libopusfile owns
// its own Ogg parsing, so there is no separate reader library to name.
func (d *libopusfileDecoder) ParserName() string { return "libopusfile (bundled)" }

func (d *libopusfileDecoder) ReadFrames(dst []float32) (int, error) {
	if d.closed {
		return 0, ErrClosed
	}
	if len(dst) < 2 {
		return 0, nil
	}

	frames := 0
	for frames*2 < len(dst) {
		if len(d.pending) == 0 {
			if err := d.fill(); err != nil {
				if errors.Is(err, ErrEndOfStream) {
					if frames == 0 {
						return 0, ErrEndOfStream
					}

					return frames, nil
				}

				return frames, err
			}
		}

		want := len(dst)/2 - frames
		n := min(len(d.pending)/2, want)
		copy(dst[frames*2:(frames+n)*2], d.pending[:n*2])
		d.pending = d.pending[n*2:]
		frames += n
		d.pos += int64(n)
	}

	return frames, nil
}

// fill reads one buffer's worth from the library, retrying across OP_HOLE which
// signals a gap in the data rather than end of stream.
func (d *libopusfileDecoder) fill() error {
	for {
		n := d.lib.readFloat(d.of, &d.buf[0], int32(len(d.buf)))
		switch {
		case n == 0:
			return ErrEndOfStream
		case n == opHole:
			continue
		case n < 0:
			return fmt.Errorf("decode: op_read_float_stereo: code %d", n)
		}

		d.pending = d.buf[:int(n)*2]

		return nil
	}
}

func (d *libopusfileDecoder) SeekFrame(frame int64) error {
	if d.closed {
		return ErrClosed
	}
	if frame < 0 {
		return fmt.Errorf("decode: negative seek target %d", frame)
	}
	if code := d.lib.pcmSeek(d.of, frame); code != 0 {
		return fmt.Errorf("decode: op_pcm_seek(%d): code %d", frame, code)
	}

	d.pending = nil
	// op_pcm_tell confirms where the library actually landed instead of trusting
	// the requested offset.
	d.pos = d.lib.pcmTell(d.of)

	return nil
}

func (d *libopusfileDecoder) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	d.pending = nil
	if d.of != 0 {
		d.lib.free(d.of)
		d.of = 0
	}

	return nil
}
