#!/usr/bin/env bash
set -euo pipefail

if [ $# -ne 1 ]; then
  echo "usage: $0 <conference>/recordings/edits/livestream/<file.mp4>" >&2
  exit 2
fi

raw_path="${1#/}"
remote="${SPACES_REMOTE:-spaces:btcpp}"
workdir="${WORKDIR:-/tmp/streamctl-transcode}"
video_bitrate="${VIDEO_BITRATE:-6800k}"
audio_bitrate="${AUDIO_BITRATE:-160k}"

case "$raw_path" in
  */recordings/edits/livestream/*) ;;
  *)
    echo "path must be <conference>/recordings/edits/livestream/<file>" >&2
    exit 2
    ;;
esac

conference="${raw_path%%/recordings/edits/livestream/*}"
filename="${raw_path##*/}"
normalized_path="${conference}/recordings/normalized/livestream/${filename}"

remote_path() {
  case "$remote" in
    *:|*/) printf '%s%s' "$remote" "$1" ;;
    *) printf '%s/%s' "$remote" "$1" ;;
  esac
}

mkdir -p "$workdir"
raw_file="${workdir}/${filename}"
out_file="${workdir}/${filename%.mp4}.normalized.mp4"
ready_file="${workdir}/${filename}.ready.json"

echo "transcode: downloading ${raw_path}"
rclone copyto "$(remote_path "$raw_path")" "$raw_file"

echo "transcode: encoding ${raw_path} -> ${normalized_path}"
rm -f -- "$out_file"
ffmpeg -hide_banner -loglevel error -stats_period 30 -progress pipe:1 -y \
  -i "$raw_file" \
  -map 0:v:0 -map 0:a:0 -dn -sn \
  -c:v h264_nvenc -preset p4 -profile:v high -pix_fmt yuv420p \
  -r 30 -g 60 -keyint_min 60 -sc_threshold 0 \
  -b:v "$video_bitrate" -maxrate "$video_bitrate" -bufsize "${VIDEO_BUFSIZE:-13600k}" \
  -c:a aac -b:a "$audio_bitrate" -ar 48000 -ac 2 \
  -movflags +faststart \
  "$out_file"

echo "transcode: verifying ${out_file}"
ffprobe -v error -show_streams "$out_file" >/dev/null

cat > "$ready_file" <<EOF
{
  "raw_path": "${raw_path}",
  "normalized_path": "${normalized_path}",
  "video_bitrate": "${video_bitrate}",
  "audio_bitrate": "${audio_bitrate}",
  "encoder": "h264_nvenc",
  "status": "ready"
}
EOF

echo "transcode: uploading ${normalized_path}"
rclone copyto "$out_file" "$(remote_path "$normalized_path")"
rclone copyto "$ready_file" "$(remote_path "${normalized_path}.ready.json")"

echo "transcode: ready ${normalized_path}"
