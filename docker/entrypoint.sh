#!/bin/sh
set -e

echo "[ClipFS] Starting..."

# fusermount3 requires the running UID to exist in /etc/passwd.
# If running as an arbitrary UID (via docker user:), add a passwd entry.
if ! whoami >/dev/null 2>&1; then
  echo "clipfs:x:$(id -u):$(id -g)::/tmp:/sbin/nologin" >> /etc/passwd
fi

exec /usr/local/bin/clipfs