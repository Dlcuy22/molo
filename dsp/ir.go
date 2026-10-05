// Clean-room note: impulse response loading and formatting in this file is an
// independent implementation based on the standard RIFF/WAVE container
// specification. It does not use or translate any GPL/LGPL source code.

package dsp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"sync"
)

var (
	errInvalidWAV        = errors.New("dsp: invalid WAV file")
	errUnsupportedFormat = errors.New("dsp: unsupported WAV format")
	errKernelNotFound    = errors.New("dsp: kernel not found")
)

// impulseResponse holds the decoded and stereo-normalized samples of an impulse response.
type impulseResponse struct {
	name       string
	sampleRate int
	frames     int
	samples    []float32
}

var (
	kernelMu       sync.RWMutex
	kernelRegistry = make(map[string]*impulseResponse)
)

func init() {
	kernelRegistry["identity"] = &impulseResponse{
		name:       "identity",
		sampleRate: 48000,
		frames:     1,
		samples:    []float32{1.0, 1.0},
	}
}

// RegisterKernel loads and registers an impulse response file under name.
func RegisterKernel(name, path string) error {
	if name == "" {
		return errors.New("dsp: kernel name cannot be empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("dsp: read kernel file %s: %w", path, err)
	}
	ir, err := parseWAV(data)
	if err != nil {
		return fmt.Errorf("dsp: parse kernel %s: %w", name, err)
	}
	ir.name = name

	kernelMu.Lock()
	kernelRegistry[name] = ir
	kernelMu.Unlock()

	return nil
}

// Kernels lists all registered kernel names in sorted order.
func Kernels() []string {
	kernelMu.RLock()
	defer kernelMu.RUnlock()

	names := make([]string, 0, len(kernelRegistry))
	for k := range kernelRegistry {
		names = append(names, k)
	}
	slices.Sort(names)

	return names
}

// kernels is an alias for Kernels matching the specification naming.
func kernels() []string {
	return Kernels()
}

func getKernel(name string) (*impulseResponse, error) {
	kernelMu.RLock()
	defer kernelMu.RUnlock()

	ir, ok := kernelRegistry[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", errKernelNotFound, name)
	}

	return ir, nil
}

// parseWAV parses a RIFF/WAVE byte stream containing PCM16, float32, or float64 samples.
func parseWAV(data []byte) (*impulseResponse, error) {
	if len(data) < 12 {
		return nil, errInvalidWAV
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, errInvalidWAV
	}

	var (
		audioFormat   uint16
		numChannels   uint16
		sampleRate    uint32
		bitsPerSample uint16
		dataOffset    = -1
		dataLen       int
		offset        = 12
	)

	for offset+8 <= len(data) {
		chunkID := string(data[offset : offset+4])
		chunkSize := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		offset += 8
		if chunkSize < 0 || offset+chunkSize > len(data) {
			if chunkID == "data" {
				chunkSize = len(data) - offset
			} else {
				return nil, errInvalidWAV
			}
		}

		switch chunkID {
		case "fmt ":
			if chunkSize < 16 {
				return nil, errInvalidWAV
			}
			audioFormat = binary.LittleEndian.Uint16(data[offset : offset+2])
			numChannels = binary.LittleEndian.Uint16(data[offset+2 : offset+4])
			sampleRate = binary.LittleEndian.Uint32(data[offset+4 : offset+8])
			bitsPerSample = binary.LittleEndian.Uint16(data[offset+14 : offset+16])
			if audioFormat == 0xFFFE && chunkSize >= 40 {
				subFormat := binary.LittleEndian.Uint16(data[offset+24 : offset+26])
				audioFormat = subFormat
			}
		case "data":
			dataOffset = offset
			dataLen = chunkSize
		}

		offset += chunkSize
		if chunkSize%2 != 0 {
			offset++
		}
	}

	if dataOffset < 0 || sampleRate == 0 || numChannels == 0 {
		return nil, errInvalidWAV
	}
	if numChannels != 1 && numChannels != 2 {
		return nil, fmt.Errorf("%w: expected 1 or 2 channels, got %d", errUnsupportedFormat, numChannels)
	}

	bytesPerSample := int(bitsPerSample / 8)
	if bytesPerSample == 0 {
		return nil, errInvalidWAV
	}
	frameBytes := int(numChannels) * bytesPerSample
	frames := dataLen / frameBytes
	if frames == 0 {
		return nil, fmt.Errorf("%w: WAV data chunk is empty", errInvalidWAV)
	}

	samples := make([]float32, frames*2)
	rawData := data[dataOffset : dataOffset+frames*frameBytes]

	switch audioFormat {
	case 1:
		if bitsPerSample != 16 {
			return nil, fmt.Errorf("%w: PCM bit depth %d not supported", errUnsupportedFormat, bitsPerSample)
		}
		if numChannels == 1 {
			for i := 0; i < frames; i++ {
				raw := int16(binary.LittleEndian.Uint16(rawData[i*2 : i*2+2]))
				s := float32(raw) / 32768.0
				samples[i*2] = s
				samples[i*2+1] = s
			}
		} else {
			for i := 0; i < frames; i++ {
				l := int16(binary.LittleEndian.Uint16(rawData[i*4 : i*4+2]))
				r := int16(binary.LittleEndian.Uint16(rawData[i*4+2 : i*4+4]))
				samples[i*2] = float32(l) / 32768.0
				samples[i*2+1] = float32(r) / 32768.0
			}
		}

	case 3:
		switch bitsPerSample {
		case 32:
			if numChannels == 1 {
				for i := 0; i < frames; i++ {
					bits := binary.LittleEndian.Uint32(rawData[i*4 : i*4+4])
					s := math.Float32frombits(bits)
					samples[i*2] = s
					samples[i*2+1] = s
				}
			} else {
				for i := 0; i < frames; i++ {
					lBits := binary.LittleEndian.Uint32(rawData[i*8 : i*8+4])
					rBits := binary.LittleEndian.Uint32(rawData[i*8+4 : i*8+8])
					samples[i*2] = math.Float32frombits(lBits)
					samples[i*2+1] = math.Float32frombits(rBits)
				}
			}
		case 64:
			if numChannels == 1 {
				for i := 0; i < frames; i++ {
					bits := binary.LittleEndian.Uint64(rawData[i*8 : i*8+8])
					s := float32(math.Float64frombits(bits))
					samples[i*2] = s
					samples[i*2+1] = s
				}
			} else {
				for i := 0; i < frames; i++ {
					lBits := binary.LittleEndian.Uint64(rawData[i*16 : i*16+8])
					rBits := binary.LittleEndian.Uint64(rawData[i*16+8 : i*16+16])
					samples[i*2] = float32(math.Float64frombits(lBits))
					samples[i*2+1] = float32(math.Float64frombits(rBits))
				}
			}
		default:
			return nil, fmt.Errorf("%w: float bit depth %d not supported", errUnsupportedFormat, bitsPerSample)
		}

	default:
		return nil, fmt.Errorf("%w: audio format code %d not supported", errUnsupportedFormat, audioFormat)
	}

	return &impulseResponse{
		sampleRate: int(sampleRate),
		frames:     frames,
		samples:    samples,
	}, nil
}

// resampleLinear resamples stereo samples to dstRate with linear interpolation.
// Linear interpolation causes high-frequency attenuation compared to sinc filtering.
func resampleLinear(samples []float32, frames, srcRate, dstRate int) ([]float32, int) {
	if srcRate == dstRate || frames <= 0 {
		out := make([]float32, len(samples))
		copy(out, samples)
		return out, frames
	}

	dstFrames := int(math.Round(float64(frames) * float64(dstRate) / float64(srcRate)))
	if dstFrames < 1 {
		dstFrames = 1
	}

	out := make([]float32, dstFrames*2)
	ratio := float64(srcRate) / float64(dstRate)

	for j := 0; j < dstFrames; j++ {
		pos := float64(j) * ratio
		i := int(math.Floor(pos))
		frac := float32(pos - float64(i))

		for c := 0; c < 2; c++ {
			var s0, s1 float32
			if i < frames {
				s0 = samples[i*2+c]
			}
			if i+1 < frames {
				s1 = samples[(i+1)*2+c]
			}
			out[j*2+c] = (1.0-frac)*s0 + frac*s1
		}
	}

	return out, dstFrames
}

// applyIRWidth adjusts stereo width using mid-side scaling on interleaved stereo samples.
func applyIRWidth(samples []float32, widthPercent float64) {
	if widthPercent == 100.0 {
		return
	}
	w := float32(widthPercent / 100.0)
	for i := 0; i+1 < len(samples); i += 2 {
		mid := (samples[i] + samples[i+1]) * 0.5
		side := (samples[i] - samples[i+1]) * 0.5
		samples[i] = mid + w*side
		samples[i+1] = mid - w*side
	}
}

// applyIRAutogain normalizes impulse response energy to 1/sqrt(power), capped at unity.
func applyIRAutogain(samples []float32) {
	var sumSq float64
	for _, s := range samples {
		sumSq += float64(s) * float64(s)
	}
	energy := sumSq * 0.5
	if energy <= 0 {
		return
	}
	scale := 1.0 / math.Sqrt(energy)
	if scale > 1.0 {
		scale = 1.0
	}
	factor := float32(scale)
	for i := range samples {
		samples[i] *= factor
	}
}
