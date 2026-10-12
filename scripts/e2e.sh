#!/usr/bin/env bash
# End-to-end test of a built image: FUSE mount with the production hardening, byte-range
# and remuxed clips decoded and compared with the source, UI edits, crash recovery and
# clean shutdown. Needs Docker, /dev/fuse and jq. Usage: scripts/e2e.sh [image]
set -euo pipefail
cd "$(dirname "$0")/.."
IMAGE=${1:-smartclips:dev}
WORK=$(mktemp -d "${RUNNER_TEMP:-/tmp}/smartclips-e2e.XXXXXX")
UID_=$(id -u)
GID_=$(id -g)
MOUNT=smartclips-e2e-mount
UI=smartclips-e2e-ui
PORT=18089

log() { printf '\n=== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; docker logs "$MOUNT" 2>&1 | tail -30 >&2 || true; exit 1; }
cleanup() {
  docker rm -f "$MOUNT" "$UI" >/dev/null 2>&1 || true
  # Detach a mount left behind by a failed run (from the parent bind, so it reaches the host).
  docker run --rm --cap-add SYS_ADMIN --security-opt apparmor=unconfined -v "$WORK/mnt:/m:rshared" \
    --entrypoint sh "$IMAGE" -c 'umount -l /m/library 2>/dev/null || true' --user 0:0 >/dev/null 2>&1 || true
  rm -rf "$WORK" 2>/dev/null || true
}
trap cleanup EXIT
tool() { docker run --rm -u "$UID_:$GID_" -v "$WORK:/w:rslave" --entrypoint "$1" "$IMAGE" "${@:2}"; }
start_mount() {
  docker run -d --name "$MOUNT" --user 0:0 -e PUID="$UID_" -e PGID="$GID_" \
    --cap-drop ALL --cap-add SYS_ADMIN --cap-add SETUID --cap-add SETGID --device /dev/fuse \
    --security-opt apparmor=unconfined --security-opt no-new-privileges=true --read-only --network none \
    -v "$WORK/data:/data:ro" -v "$WORK/media:/media:ro" -v "$WORK/cache:/cache" \
    -v "$WORK/mnt:/mnt/smartclips:rshared" "$IMAGE" mount >/dev/null
}
wait_for() { # wait_for <description> <command...>
  local what=$1; shift
  for _ in $(seq 1 120); do "$@" >/dev/null 2>&1 && return 0; sleep 0.5; done
  fail "timed out waiting for $what"
}

log "fixtures"
mkdir -p "$WORK"/{data,media,cache,mnt}
cp testdata/sample.mkv "$WORK/media/sample.mkv"
tool ffmpeg -nostdin -v error -i /w/media/sample.mkv -map 0:v -map 0:a -c copy -tag:v hvc1 /w/media/sample.mp4
cat > "$WORK/data/clips.json" <<'JSON'
[
  {"input": "sample.mkv", "output": "Byte range", "group": "E2E", "start": 5.5, "end": 17,
   "nfo_type": "musicvideo", "metadata": {"title": "Byte range", "year": 2024}},
  {"input": "sample.mp4", "output": "Remuxed", "group": "E2E", "start": 4, "end": 10},
  {"input": "../../etc/passwd", "output": "Escape", "start": 0, "end": 5}
]
JSON

log "mount"
start_mount
wait_for "clips" test -f "$WORK/mnt/library/E2E/Byte range.mkv" -a -f "$WORK/mnt/library/E2E/Remuxed.mkv"
ls -la "$WORK/mnt/library/E2E"
[[ -f "$WORK/mnt/library/E2E/Byte range.nfo" ]] || fail "no NFO"
[[ ! -e "$WORK/mnt/library/Escape.mkv" ]] || fail "invalid clip was mounted"
wait_for "status file" jq -e '.Escape.state == "error"' "$WORK/cache/status.json"
[[ $(jq -r '."E2E/Byte range".kind' "$WORK/cache/status.json") == mkv ]] || fail "byte-range clip was not served by reference"
[[ $(jq -r '."E2E/Remuxed".kind' "$WORK/cache/status.json") == remux ]] || fail "mp4 clip was not remuxed"
uid=$(docker exec "$MOUNT" awk '/^Uid:/{print $2, $3}' /proc/1/status)
[[ $uid == "$UID_ $UID_" ]] || fail "mount service still runs as $uid"

log "decode and compare with the source"
for clip in "Byte range" "Remuxed"; do
  tool ffmpeg -nostdin -v error -xerror -i "/w/mnt/library/E2E/$clip.mkv" -map 0:v -map 0:a -f null - \
    || fail "$clip does not decode"
done
origin=$(jq -r '."E2E/Byte range".actual_start' "$WORK/cache/status.json")
frames() { tool ffmpeg -nostdin -v error -ss "$2" -i "$1" -map 0:v -frames:v 24 -f framemd5 - | grep -v '^#' | awk -F, '{print $6}'; }
for off in 1 6; do
  a=$(frames "/w/mnt/library/E2E/Byte range.mkv" "$off")
  b=$(frames /w/media/sample.mkv "$(awk "BEGIN{print $origin + $off}")")
  [[ -n $a && $a == "$b" ]] || fail "frames at +${off}s differ from the source"
done
echo "frames match the source"

log "crash recovery"
docker kill "$MOUNT" >/dev/null
if ls "$WORK/mnt/library" >/dev/null 2>&1; then fail "expected a dead mount after SIGKILL"; fi
docker rm "$MOUNT" >/dev/null
start_mount
wait_for "remount" test -f "$WORK/mnt/library/E2E/Byte range.mkv"
docker logs "$MOUNT" 2>&1 | grep -q "detaching stale mount" || fail "stale mount was not detected"
wait_for "healthy" sh -c "[ \"\$(docker inspect -f '{{.State.Health.Status}}' $MOUNT)\" = healthy ]"
echo "recovered and healthy"

log "ui"
chmod 0777 "$WORK/data"
docker run -d --name "$UI" --user "$UID_:$GID_" --cap-drop ALL --security-opt no-new-privileges=true \
  --read-only --tmpfs /tmp -p "127.0.0.1:$PORT:8080" \
  -v "$WORK/data:/data" -v "$WORK/media:/media:ro" -v "$WORK/cache:/cache:ro" "$IMAGE" ui >/dev/null
wait_for "ui" curl -fsS "http://127.0.0.1:$PORT/healthz"
state=$(curl -fsS "http://127.0.0.1:$PORT/api/clips")
body=$(jq -c '{version, clips: ([.clips[] | select(.output != "Escape")] + [{"input": "sample.mkv", "output": "Added", "start": 20, "end": 26}])}' <<< "$state")
code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/json' --data "$body" "http://127.0.0.1:$PORT/api/clips")
[[ $code == 200 ]] || fail "UI save returned $code"
code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/json' --data "$body" "http://127.0.0.1:$PORT/api/clips")
[[ $code == 409 ]] || fail "stale save returned $code, want 409"
wait_for "hot reload" test -f "$WORK/mnt/library/Added.mkv"
tool ffmpeg -nostdin -v error -xerror -i /w/mnt/library/Added.mkv -f null - || fail "added clip does not decode"
echo "UI edit appeared in the mount"

log "graceful stop"
docker stop -t 10 "$MOUNT" >/dev/null
if grep -q " $WORK/mnt/library " /proc/self/mountinfo; then fail "mount left behind after stop"; fi
echo "unmounted cleanly"

log "PASS"
