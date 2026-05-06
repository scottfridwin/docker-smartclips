# docker-smartclips

A Docker container that exposes virtual video clips via a FUSE filesystem. Define clip segments in a JSON config and they appear as regular `.mkv` files, generated on-the-fly by ffmpeg.

## How it works

ClipFS mounts a FUSE filesystem inside the container. Each entry in your `clips.json` becomes a virtual file that, when read, extracts the specified time range from the source media using `ffmpeg -c copy` (no re-encoding).

## Quick Start

```yaml
services:
  smartclips:
    image: ghcr.io/scottfridwin/docker-smartclips:latest
    container_name: smartclips
    network_mode: none
    read_only: true
    user: "1000:1000"
    cap_add:
      - SYS_ADMIN
    devices:
      - /dev/fuse:/dev/fuse
    security_opt:
      - apparmor:unconfined
    tmpfs:
      - /tmp
    volumes:
      - ./clips.json:/config/clips.json:ro
      - /path/to/media:/media:ro
      - /mnt/smartclips:/mnt/clipfs:rshared
      - smartclips-cache:/cache
    environment:
      - CLIPFS_CONFIG=/config/clips.json
      - CLIPFS_MOUNT=/mnt/clipfs
      - CLIPFS_MEDIA=/media
      - CLIPFS_CACHE_DIR=/cache
      - CLIPFS_CACHE_MAX_MB=2048
    restart: unless-stopped

volumes:
  smartclips-cache:
```

> **Important:** The host-side mount (`/mnt/smartclips`) must use `:rshared` propagation so the FUSE mount inside the container is visible on the host.

You may also need to run once on the host:
```bash
sudo mount --make-shared /mnt/smartclips
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `CLIPFS_CONFIG` | `clips.json` | Path to the clip definitions JSON file |
| `CLIPFS_MOUNT` | `/mnt/clipfs` | Mountpoint for the FUSE filesystem |
| `CLIPFS_MEDIA` | *(empty)* | If set, prepended to relative `input` paths in clips.json |
| `CLIPFS_CACHE_DIR` | `/tmp/clipfs-cache` | Directory for the disk-backed clip cache |
| `CLIPFS_CACHE_MAX_MB` | `1024` | Maximum cache size in MB. LRU eviction when exceeded. |

File ownership on virtual clips matches the `user:` specified in your compose file.

## clips.json Format

```json
[
    {
        "input": "/media/movie.mkv",
        "output": "scene1",
        "start": 120,
        "end": 240
    },
    {
        "input": "movie.mkv",
        "output": "scene2",
        "start": 300,
        "end": 360
    }
]
```

| Field | Description |
|-------|-------------|
| `input` | Source media file path (any format ffmpeg supports). Absolute paths are used as-is; relative paths are prefixed with `CLIPFS_MEDIA`. |
| `output` | Base filename (without extension). `.mkv` is appended automatically. |
| `start` | Start time in seconds |
| `end` | End time in seconds |

> **Note:** All output clips are muxed into Matroska (`.mkv`) format regardless of the input format. MKV is used because it supports virtually all codec combinations and allows proper seek indexing.

## Required Docker Permissions

The container needs these capabilities to mount FUSE:

- `cap_add: SYS_ADMIN` — required for the `mount` syscall
- `devices: /dev/fuse` — the FUSE device node
- `security_opt: apparmor:unconfined` — bypass AppArmor restrictions on mount (if applicable)

## Building

```bash
docker build -t docker-smartclips .
```

The image supports multi-architecture builds (amd64, arm64) via Docker BuildKit's `TARGETARCH`.