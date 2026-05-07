# docker-smartclips

A Docker container that exposes virtual video clips via a FUSE filesystem. Define clip segments in a JSON config and they appear as regular `.mkv` files, generated on-the-fly by ffmpeg.

## How it works

SmartClips mounts a FUSE filesystem inside the container. Each entry in your `clips.json` becomes a virtual file that, when read, extracts the specified time range from the source media using `ffmpeg -c copy` (no re-encoding).

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
      - /mnt/smartclips:/mnt/smartclips:rshared
      - smartclips-cache:/cache
    environment:
      - SMARTCLIPS_CONFIG=/config/clips.json
      - SMARTCLIPS_MOUNT=/mnt/smartclips
      - SMARTCLIPS_MEDIA=/media
      - SMARTCLIPS_CACHE_DIR=/cache
      - SMARTCLIPS_CACHE_MAX_MB=2048
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
| `SMARTCLIPS_CONFIG` | `clips.json` | Path to the clip definitions JSON file |
| `SMARTCLIPS_MOUNT` | `/mnt/smartclips` | Mountpoint for the FUSE filesystem |
| `SMARTCLIPS_MEDIA` | *(empty)* | If set, prepended to relative `input` paths in clips.json |
| `SMARTCLIPS_CACHE_DIR` | `/tmp/smartclips-cache` | Directory for the disk-backed clip cache |
| `SMARTCLIPS_CACHE_MAX_MB` | `1024` | Maximum cache size in MB. LRU eviction when exceeded. |

File ownership on virtual clips matches the `user:` specified in your compose file.

## clips.json Format

```json
[
    {
        "input": "/media/chicago.mkv",
        "output": "Cell Block Tango",
        "start": 1800.0,
        "end": 2100.0,
        "group": "Chicago (2002)",
        "nfo_type": "musicvideo",
        "metadata": {
            "title": "Cell Block Tango",
            "artist": "Cast",
            "album": "Chicago (2002)",
            "genre": ["Musical", "Drama"],
            "year": "2002",
            "plot": "The six merry murderesses tell their tales"
        }
    },
    {
        "input": "/media/b99-s03e05.mkv",
        "output": "Cold Open - The Heist",
        "start": 0.0,
        "end": 180.5,
        "group": "Brooklyn Nine-Nine/Season 03",
        "nfo_type": "episodedetails",
        "metadata": {
            "title": "Cold Open - The Heist",
            "showtitle": "Brooklyn Nine-Nine",
            "season": "3",
            "episode": "5",
            "genre": ["Comedy"]
        }
    },
    {
        "input": "movie.mkv",
        "output": "Blooper Reel",
        "start": 7200,
        "end": 7500
    }
]
```

| Field | Description |
|-------|-------------|
| `input` | Source media file path (any format ffmpeg supports). Absolute paths are used as-is; relative paths are prefixed with `SMARTCLIPS_MEDIA`. |
| `output` | Base filename (without extension). `.mkv` is appended automatically. |
| `start` | Start time in seconds (supports decimals for sub-second precision) |
| `end` | End time in seconds |
| `group` | *(optional)* Directory path using `/` as separator. Creates nested folders in the FUSE tree. |
| `nfo_type` | *(optional)* NFO root element type: `movie` (default), `episodedetails`, `musicvideo` |
| `metadata` | *(optional)* Free-form map serialized directly into an NFO XML file alongside the clip |

### Metadata field types

- **String** → `<key>value</key>`
- **Number** → `<key>42</key>`
- **Array of strings** → repeated elements: `"genre": ["A", "B"]` → `<genre>A</genre><genre>B</genre>`
- **Array of objects** → nested repeated elements: `"actor": [{"name": "X", "role": "Y"}]` → `<actor><name>X</name><role>Y</role></actor>`

### Resulting FUSE tree

```
/mnt/smartclips/
  Chicago (2002)/
    Cell Block Tango.mkv
    Cell Block Tango.nfo
  Brooklyn Nine-Nine/
    Season 03/
      Cold Open - The Heist.mkv
      Cold Open - The Heist.nfo
  Blooper Reel.mkv
```

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