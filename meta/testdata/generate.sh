#!/usr/bin/env bash
# Regenerates the metadata fixtures used by the meta tests.
#
# tagged.opus is a short Opus tone carrying ID3-style Vorbis comments and an
# 8x8 PNG cover, so the embedded resolver has a real title, numbers and art to
# read. cover.png is the expected extraction of that artwork.
set -euo pipefail

cd "$(dirname "$0")"

python3 - <<'PY'
import struct, zlib

w = h = 8
raw = b""
for _ in range(h):
    raw += b"\x00" + bytes([255, 0, 0] * w)

def chunk(tag, data):
    return struct.pack(">I", len(data)) + tag + data + struct.pack(">I", zlib.crc32(tag + data) & 0xffffffff)

png = (b"\x89PNG\r\n\x1a\n"
       + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
       + chunk(b"IDAT", zlib.compress(raw))
       + chunk(b"IEND", b""))
open("cover.png", "wb").write(png)
PY

ffmpeg -v error -y -f lavfi -i "sine=frequency=300:sample_rate=48000:duration=0.3" -ac 2 tone.wav
# --serial pins the Ogg stream id, which opusenc otherwise randomises; without
# it the fixture bytes (and so the committed file) differ on every run.
opusenc --quiet --bitrate 32 --serial 0 \
  --title "Tagged Title" \
  --artist "Tag Artist" \
  --album "Tag Album" \
  --date "2021" \
  --comment "TRACKNUMBER=3" \
  --comment "DISCNUMBER=1" \
  --comment "ALBUMARTIST=Tag Album Artist" \
  --picture cover.png \
  tone.wav tagged.opus
rm -f tone.wav

ls -l tagged.opus cover.png
