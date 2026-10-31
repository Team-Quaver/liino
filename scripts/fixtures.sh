#!/usr/bin/env bash
# Generate the small audio fixtures the native decoder tests need.
# They are not committed: run this once, then `go test ./...` picks them up.
#
# Channels carry different frequencies on purpose: a decoder that mixes up
# planes or channel counts still produces sound, but it no longer passes the
# per-channel assertions in decode/native.
set -euo pipefail
dir="${1:-$(dirname "$0")/../testdata}"
mkdir -p "$dir"
command -v ffmpeg >/dev/null || { echo "ffmpeg is required" >&2; exit 1; }

tone() { # tone <exprs> <rate> <seconds>
	local exprs=$1 rate=$2 secs=$3
	echo "aevalsrc=${exprs}:d=${secs}:s=${rate}"
}

# 44.1kHz stereo MP3: resampled to the canonical 48kHz stereo.
ffmpeg -v error -y -f lavfi -i "$(tone '.3*sin(2*PI*440*t)|.3*sin(2*PI*880*t)' 44100 3)" \
	-ac 2 -channel_layout stereo -c:a libmp3lame -b:a 128k "$dir/tone-44100-stereo.mp3"

# 96kHz mono FLAC: downsampled and duplicated onto both ears.
ffmpeg -v error -y -f lavfi -i "$(tone '.3*sin(2*PI*880*t)' 96000 2)" \
	-ac 1 -channel_layout mono -c:a flac "$dir/tone-96000-mono.flac"

# 48kHz stereo Ogg/Vorbis: native rate, read through an in-memory reader.
ffmpeg -v error -y -f lavfi -i "$(tone '.25*sin(2*PI*220*t)|.25*sin(2*PI*330*t)' 48000 2)" \
	-ac 2 -channel_layout stereo -c:a libvorbis -q 5 "$dir/tone-48000-stereo.ogg"

# 5.1 FLAC: exercises the multichannel path.
ffmpeg -v error -y -f lavfi -i \
	"$(tone '.15*sin(2*PI*220*t)|.15*sin(2*PI*330*t)|.15*sin(2*PI*440*t)|.15*sin(2*PI*550*t)|.15*sin(2*PI*660*t)|.15*sin(2*PI*770*t)' 48000 2)" \
	-ac 6 -channel_layout 5.1 -c:a flac "$dir/tone-48000-51.flac"

# 7.1.4 Opus: QQ Music's "臻品全景声 7.1" shape. 12 channels with mapping
# family 3 leave AVFrame.ch_layout unspecified; libswresample must not be asked
# to rematrix it (see decode/native). opusenc is optional for this fixture.
if command -v opusenc >/dev/null; then
	ffmpeg -v error -y -f lavfi -i \
		"$(tone '.12*sin(2*PI*220*t)|.12*sin(2*PI*330*t)|.12*sin(2*PI*440*t)|.12*sin(2*PI*550*t)|.12*sin(2*PI*660*t)|.12*sin(2*PI*770*t)|.12*sin(2*PI*880*t)|.12*sin(2*PI*990*t)|.10*sin(2*PI*1100*t)|.10*sin(2*PI*1210*t)|.10*sin(2*PI*1320*t)|.10*sin(2*PI*1430*t)' 48000 1)" \
		-ac 12 -channel_layout 7.1.4 -c:a pcm_f32le /tmp/tone-48000-71_4.wav
	opusenc --quiet /tmp/tone-48000-71_4.wav "$dir/tone-48000-71_4.opus"
	rm -f /tmp/tone-48000-71_4.wav
else
	echo "opusenc not found; skipping tone-48000-71_4.opus" >&2
fi

for f in "$dir"/tone-*; do
	printf '%s\n' "$(basename "$f"): $(ffprobe -v error -show_entries stream=sample_rate,channels,channel_layout -of csv=p=0 "$f")"
done
