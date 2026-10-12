#!/usr/bin/env bash
# Regenerates testdata/sample.mkv: 30 s of HEVC (B-frames, keyframe every 2 s), AAC and
# SRT subtitles, muxed by ffmpeg like the media library's files. Uses the project image's ffmpeg.
set -euo pipefail
cd "$(dirname "$0")/.."
image=smartclips:fixtures
docker build -q -f build/Dockerfile -t "$image" . >/dev/null
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
for i in $(seq 0 14); do
  printf '%d\n00:00:%02d,000 --> 00:00:%02d,500\nLine %d\n\n' $((i + 1)) $((i * 2)) $((i * 2 + 1)) $((i + 1))
done > "$work/subs.srt"
docker run --rm -u "$(id -u):$(id -g)" -v "$work:/w" --entrypoint ffmpeg "$image" \
  -nostdin -hide_banner -loglevel error \
  -f lavfi -i "testsrc2=size=192x108:rate=24:duration=30" \
  -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=30" \
  -i /w/subs.srt \
  -map 0:v -map 1:a -map 2:s \
  -c:v libx265 -preset ultrafast -crf 38 -x265-params "keyint=48:min-keyint=48:scenecut=0:bframes=3:log-level=error" \
  -c:a aac -b:a 32k -ac 1 -c:s srt \
  -metadata title="SmartClips fixture" -fflags +bitexact -flags:v +bitexact -flags:a +bitexact \
  -f matroska -y /w/sample.mkv
cp "$work/sample.mkv" testdata/sample.mkv
ls -l testdata/sample.mkv
