#!/usr/bin/env bash
set -euo pipefail

if [ $# -ne 1 ]; then
  echo "usage: $0 <conference>/recordings/edits/livestream/<file.mp4>" >&2
  exit 2
fi

raw_path="${1#/}"
remote="${SPACES_REMOTE:-spaces:btcpp}"
workroot="${WORKDIR:-/tmp/streamctl-transcode}"
video_bitrate="${VIDEO_BITRATE:-6800k}"
audio_bitrate="${AUDIO_BITRATE:-160k}"
rclone_stats="${RCLONE_STATS:-30s}"
rclone_multithread_streams="${RCLONE_MULTI_THREAD_STREAMS:-16}"
rclone_multithread_cutoff="${RCLONE_MULTI_THREAD_CUTOFF:-256M}"
rclone_transfers="${RCLONE_TRANSFERS:-1}"
rclone_checkers="${RCLONE_CHECKERS:-8}"

case "$raw_path" in
  */recordings/edits/*) ;;
  *)
    echo "path must be <conference>/recordings/edits/<path>/<file>" >&2
    exit 2
    ;;
esac

conference="${raw_path%%/recordings/edits/*}"
relative_path="${raw_path#${conference}/recordings/edits/}"
filename="${raw_path##*/}"
normalized_path="${conference}/recordings/normalized/${relative_path}"

remote_path() {
  case "$remote" in
    *:|*/) printf '%s%s' "$remote" "$1" ;;
    *) printf '%s/%s' "$remote" "$1" ;;
  esac
}

safe_clean_workroot() {
  case "$workroot" in
    /tmp/streamctl-transcode|/tmp/streamctl-transcode/*) ;;
    *)
      echo "refusing to clean unsafe WORKDIR: $workroot" >&2
      exit 2
      ;;
  esac
  mkdir -p "$workroot"
  find "$workroot" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
}

rclone_copyto() {
  rclone copyto \
    --stats "$rclone_stats" \
    --stats-one-line \
    --transfers "$rclone_transfers" \
    --checkers "$rclone_checkers" \
    "$@"
}

rclone_download() {
  rclone copyto \
    --stats "$rclone_stats" \
    --stats-one-line \
    --transfers 1 \
    --checkers "$rclone_checkers" \
    --multi-thread-streams "$rclone_multithread_streams" \
    --multi-thread-cutoff "$rclone_multithread_cutoff" \
    "$@"
}

export RCLONE_CONFIG="${RCLONE_CONFIG:-/root/rclone.conf}"
safe_clean_workroot
workdir="${workroot}/${filename}.job-$$"
mkdir -p "$workdir"
cleanup() {
  rm -rf -- "$workdir"
}
trap cleanup EXIT
raw_file="${workdir}/${filename}"
out_file="${workdir}/${filename%.mp4}.normalized.mp4"
ready_file="${workdir}/${filename}.ready.json"

echo "transcode: downloading ${raw_path}"
rclone_download "$(remote_path "$raw_path")" "$raw_file"

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
  "verified_by": "ffprobe",
  "status": "ready"
}
EOF

echo "transcode: uploading ${normalized_path}"
rclone_copyto "$out_file" "$(remote_path "$normalized_path")"
rclone_copyto "$ready_file" "$(remote_path "${normalized_path}.ready.json")"

echo "transcode: ready ${normalized_path}"
