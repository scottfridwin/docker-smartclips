#!/bin/sh
set -e

echo "[ClipFS] Starting..."

# ensure mount directory exists
mkdir -p "${CLIPFS_MOUNT:-/mnt/clipfs}"

exec /usr/local/bin/clipfs