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
FLAC=(-c:a flac -fflags +bitexact)

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

# Lossy and container fixtures. They use the same 440/660 Hz stereo source as
# the FLAC files, so a decoded tone can be compared across codecs. Lossy
# encoders are not bit-exact, so these double as quality oracles only: the tests
# assert a real tone at the right level, not the exact samples.
#
# -map_metadata -1 strips the encoder tag, so re-running on a different ffmpeg
# build produces the same bytes rather than a new vendor string.
BASE=(-fflags +bitexact -map_metadata -1)

"${FFMPEG[@]}" \
  -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=0.25" \
  -f lavfi -i "sine=frequency=660:sample_rate=48000:duration=0.25" \
  -filter_complex "[0:a][1:a]amerge=inputs=2,volume=4.0[a]" \
  -map "[a]" "${BASE[@]}" -c:a aac -b:a 128k -f adts sine_stereo_48k.aac

"${FFMPEG[@]}" \
  -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=0.25" \
  -f lavfi -i "sine=frequency=660:sample_rate=48000:duration=0.25" \
  -filter_complex "[0:a][1:a]amerge=inputs=2,volume=4.0[a]" \
  -map "[a]" "${BASE[@]}" -c:a aac -b:a 128k sine_stereo_48k.m4a

"${FFMPEG[@]}" \
  -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=0.25" \
  -f lavfi -i "sine=frequency=660:sample_rate=48000:duration=0.25" \
  -filter_complex "[0:a][1:a]amerge=inputs=2,volume=4.0[a]" \
  -map "[a]" "${BASE[@]}" -c:a libmp3lame -b:a 128k sine_stereo_48k.mp3

# WAV is lossless PCM, so these are bit-exactness oracles like the FLAC files.
# 16-bit 48 kHz is the common case, 24-bit proves the width scaling keeps the
# low bits, and 44.1 kHz exercises rate conversion.
WAV=(-c:a pcm_s16le)

"${FFMPEG[@]}" \
  -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=0.25" \
  -f lavfi -i "sine=frequency=660:sample_rate=48000:duration=0.25" \
  -filter_complex "[0:a][1:a]amerge=inputs=2,volume=4.0[a]" \
  -map "[a]" "${BASE[@]}" "${WAV[@]}" sine_stereo_48k.wav

"${FFMPEG[@]}" \
  -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=0.25" \
  -f lavfi -i "sine=frequency=660:sample_rate=48000:duration=0.25" \
  -filter_complex "[0:a][1:a]amerge=inputs=2,volume=4.0[a]" \
  -map "[a]" "${BASE[@]}" -c:a pcm_s24le sine_stereo_24bit.wav

"${FFMPEG[@]}" \
  -f lavfi -i "sine=frequency=440:sample_rate=44100:duration=0.25" \
  -f lavfi -i "sine=frequency=660:sample_rate=44100:duration=0.25" \
  -filter_complex "[0:a][1:a]amerge=inputs=2,volume=4.0[a]" \
  -map "[a]" "${BASE[@]}" "${WAV[@]}" sine_stereo_44k.wav

ls -l ./*.opus ./*.flac ./*.aac ./*.m4a ./*.mp3 ./*.wav
