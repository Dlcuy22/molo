#!/usr/bin/env bash
# Regenerates the Opus fixtures used by the decode tests.
#
# Sources are deterministic sine tones and the muxer runs in bitexact mode with
# a pinned stream serial, so re-running this script reproduces the fixtures
# byte for byte on a given libopus build.
set -euo pipefail

cd "$(dirname "$0")"

FFMPEG=(ffmpeg -v error -y)
ENCODE=(-c:a libopus -vbr on -application audio -serial_offset 0 -fflags +bitexact)

"${FFMPEG[@]}" \
  -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=2" \
  -f lavfi -i "sine=frequency=660:sample_rate=48000:duration=2" \
  -filter_complex "[0:a][1:a]amerge=inputs=2,volume=4.0[a]" \
  -map "[a]" "${ENCODE[@]}" -b:a 128k stereo_2s.opus

"${FFMPEG[@]}" \
  -f lavfi -i "sine=frequency=523.25:sample_rate=48000:duration=1" \
  -af volume=4.0 "${ENCODE[@]}" -b:a 64k mono_1s.opus

"${FFMPEG[@]}" \
  -f lavfi -i "sine=frequency=220:sample_rate=48000:duration=0.25" \
  -f lavfi -i "sine=frequency=330:sample_rate=48000:duration=0.25" \
  -filter_complex "[0:a][1:a]amerge=inputs=2,volume=4.0[a]" \
  -map "[a]" "${ENCODE[@]}" -b:a 96k short_stereo.opus

# FLAC fixtures. FLAC is lossless, so these double as bit-exactness oracles: a
# decoder either reproduces the samples or it is wrong. 48 kHz 16-bit is the
# native path, 44.1 kHz exercises rate conversion, 24-bit exercises wide
# samples, and mono exercises channel duplication. The 16- and 24-bit stereo
# files are the same signal at the same level, so their decoded PCM must match
# sample for sample.
FLAC=(--noprogress -c:a flac -fflags +bitexact)

"${FFMPEG[@]}" \
  -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=0.25" \
  -f lavfi -i "sine=frequency=660:sample_rate=48000:duration=0.25" \
  -filter_complex "[0:a][1:a]amerge=inputs=2,volume=4.0[a]" \
  -map "[a]" "${FLAC[@]}" -sample_fmt s16 sine_stereo_48k.flac

"${FFMPEG[@]}" \
  -f lavfi -i "sine=frequency=440:sample_rate=44100:duration=0.25" \
  -f lavfi -i "sine=frequency=660:sample_rate=44100:duration=0.25" \
  -filter_complex "[0:a][1:a]amerge=inputs=2,volume=4.0[a]" \
  -map "[a]" "${FLAC[@]}" -sample_fmt s16 sine_stereo_44k.flac

"${FFMPEG[@]}" \
  -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=0.25" \
  -f lavfi -i "sine=frequency=660:sample_rate=48000:duration=0.25" \
  -filter_complex "[0:a][1:a]amerge=inputs=2,volume=4.0[a]" \
  -map "[a]" "${FLAC[@]}" -sample_fmt s32 -bits_per_raw_sample 24 sine_stereo_24bit.flac

"${FFMPEG[@]}" \
  -f lavfi -i "sine=frequency=523.25:sample_rate=48000:duration=0.25" \
  -af volume=4.0 "${FLAC[@]}" -sample_fmt s16 sine_mono_48k.flac

ls -l ./*.opus ./*.flac
