#!/bin/sh
set -e

echo "[SmartClips] Starting..."

# fusermount3 requires the running UID to exist in /etc/passwd.
# If running as an arbitrary UID (via docker user:), add a passwd entry.
if ! whoami >/dev/null 2>&1; then
  echo "smartclips:x:$(id -u):$(id -g)::/tmp:/sbin/nologin" >> /etc/passwd
fi

# Clean up any stale FUSE mount left over from a previous run.
# Without this, the host-side bind mount becomes permanently stuck.
MOUNT="${SMARTCLIPS_MOUNT:-/mnt/smartclips}"
if mountpoint -q "$MOUNT" 2>/dev/null; then
  echo "[SmartClips] Cleaning stale mount at $MOUNT"
  fusermount3 -uz "$MOUNT" 2>/dev/null || fusermount -uz "$MOUNT" 2>/dev/null || umount -l "$MOUNT" 2>/dev/null || true
fi

exec /usr/local/bin/smartclips