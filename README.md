# SmartClips

[![Build](https://img.shields.io/github/actions/workflow/status/scottfridwin/docker-smartclips/build.yml?branch=main&label=build)](https://github.com/scottfridwin/docker-smartclips/actions/workflows/build.yml)
[![Release](https://img.shields.io/github/v/release/scottfridwin/docker-smartclips)](https://github.com/scottfridwin/docker-smartclips/releases/latest)
[![Image](https://img.shields.io/badge/image-ghcr.io-blue?logo=docker)](https://github.com/scottfridwin/docker-smartclips/pkgs/container/smartclips)
[![License](https://img.shields.io/github/license/scottfridwin/docker-smartclips)](LICENSE)

Virtual video clips for Jellyfin (or any media server). Define a clip as "this file, from here to
there", and SmartClips makes it appear as a real `.mkv` file in a folder your media server indexes,
without copying the video. Scenes, songs from musicals, cold opens: all browsable and playable like
normal library items, using no extra disk space.

> [!NOTE]
> **AI disclosure:** This project is built and maintained with substantial help from AI coding assistants (GitHub Copilot). AI is used to write and modify the code, tests, documentation and CI configuration, and to manage the repository. Dependency updates are merged and released automatically, without human review, when the automated tests pass. Review the code and test it in your own environment before relying on it.

## How it works

SmartClips serves a read-only [FUSE](https://www.kernel.org/doc/html/latest/filesystems/fuse.html)
filesystem. For Matroska sources (`.mkv`, `.webm`), a clip is assembled on the fly from the source
file's own bytes: SmartClips writes a small new header and index (a few KB) and then references the
source's clusters (its blocks of audio, video and subtitle frames) byte for byte. Nothing is
re-encoded, copied or generated when the clip is opened.

- **Instant** — a clip's layout is computed once (tens of milliseconds) and saved in an index, so
  restarts and library scans don't redo it. Opening a clip doesn't run ffmpeg.
- **Exact sizes, stable timestamps** — every clip reports its real size and a fixed modification
  time, so the media server probes it once and doesn't treat it as changed.
- **Full speed** — reads go straight to the source file. On a pooled HDD array, `ffprobe` of a clip
  takes about 0.07 s and reads run at ~150 MB/s.
- **Identical frames** — clips contain the source's own audio, video and subtitle data.
- **Keyframe starts** — a byte-range clip starts at the last keyframe at or before the requested
  start (up to one GOP earlier; about 10 s for typical encodes). The UI shows the real start and can
  snap to keyframes. Ends are cut at the requested time.
- **Non-Matroska sources** (MP4, AVI, TS, ...) work too, but each clip is remuxed into the cache
  once, ahead of time, so it uses disk space equal to the clip.
- **Hot reload** — edit `clips.json` (or use the UI) and clips appear, change or disappear within a
  second; unchanged clips are left alone. If a source file is replaced (for example by a transcode),
  its clips are rebuilt automatically.

## Quick start

```yaml
services:
  smartclips:
    image: ghcr.io/scottfridwin/smartclips:2
    container_name: smartclips
    command: mount
    # Starts as root only to mount, then runs as PUID:PGID (which must be able to read the media).
    user: "0:0"
    environment:
      - PUID=1000
      - PGID=1000
    cap_drop: [ALL]
    cap_add: [SYS_ADMIN, SETUID, SETGID]
    devices:
      - /dev/fuse:/dev/fuse
    security_opt:
      - apparmor=unconfined  # Docker's default AppArmor profile blocks mount(2)
      - no-new-privileges=true
    read_only: true
    network_mode: none
    volumes:
      - ./data:/data:ro              # clips.json
      - /path/to/media:/media:ro
      - ./cache:/cache               # layout index and remuxed clips
      - /srv/smartclips:/mnt/smartclips:rshared
    restart: unless-stopped

  smartclips-ui:
    image: ghcr.io/scottfridwin/smartclips:2
    container_name: smartclips-ui
    command: ui
    user: "1000:1000"
    cap_drop: [ALL]
    security_opt:
      - no-new-privileges=true
    read_only: true
    tmpfs:
      - /tmp                         # preview rendering
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - ./data:/data                 # writes clips.json
      - /path/to/media:/media:ro
      - ./cache:/cache:ro            # reads clip status
    restart: unless-stopped
```

The clips appear on the host in `/srv/smartclips/library`. Bind-mount `/srv/smartclips` into your
media server with `rslave` (or `rshared`) propagation and add `/srv/smartclips/library` as a library:

```yaml
  jellyfin:
    volumes:
      - /srv/smartclips:/smartclips:rslave
```

> [!IMPORTANT]
> Bind the **parent** directory (`/srv/smartclips`), not `library/` itself, into both containers.
> SmartClips mounts into `library/` below the shared bind mount, which is what lets it clean up
> after a crash: if the bind mount itself were the mount point, a crashed instance's dead mount
> would stay visible on the host until it was unmounted by hand.

The UI has **no authentication**. Keep it on localhost or put it behind an authenticating reverse
proxy (the API can change which media files are exposed as clips).

### Jellyfin tips

- Use the folder layout your library type expects (for example `Show/Season 01/Name.mkv` with
  `episodedetails` NFOs, or a flat folder of `musicvideo` NFOs in a Music Videos library).
- Turn off **real-time monitoring** for this library: FUSE filesystems don't send inotify events.
  New clips show up on the next scan (scheduled, or *Scan library files*).
- Thumbnails, trickplay and chapter images work as for any other file.

## Clips file

`clips.json` is a JSON array. The UI edits it for you; it can also be edited by hand.

```json
[
  {
    "input": "Movies/Chicago (2002)/Chicago (2002).mkv",
    "output": "Cell Block Tango",
    "start": 1800,
    "end": 2100,
    "group": "Chicago (2002)",
    "nfo_type": "musicvideo",
    "metadata": {
      "title": "Cell Block Tango",
      "artist": "Cast",
      "year": 2002,
      "genre": ["Musical", "Drama"]
    }
  }
]
```

| Field | Description |
| --- | --- |
| `input` | Source file, relative to the media folder (absolute paths must be inside it). |
| `output` | File name without extension. `.mkv` is added. |
| `start`, `end` | Seconds (decimals allowed). Clips can be at most 6 hours long. |
| `group` | Optional folder path, using `/` between folders. |
| `nfo_type` | Optional NFO root element: `movie`, `episodedetails` or `musicvideo`. |
| `metadata` | Optional object written to `<output>.nfo`. Strings, numbers and booleans become elements, arrays repeat the element, and nested objects nest (for example `"actor": [{"name": "X", "role": "Y"}]`). Keys must be valid XML names. |

Invalid clips (unsafe names, paths outside the media folder, missing files, bad times) are skipped
and reported in the log and the UI; the rest are served.

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `SMARTCLIPS_CONFIG` | `/data/clips.json` | The clips file. A missing file means no clips. |
| `SMARTCLIPS_MEDIA` | `/media` | Media folder. Inputs are confined to it. |
| `SMARTCLIPS_MOUNT` | `/mnt/smartclips/library` | Where the clips are mounted (`mount`). |
| `SMARTCLIPS_CACHE_DIR` | `/cache` | Layout index, remuxed clips and `status.json`. |
| `SMARTCLIPS_WORKERS` | `2` | Clips built in parallel (`mount`). |
| `SMARTCLIPS_RECHECK_MINUTES` | `15` | How often to re-read the clips file and check sources for changes (`mount`). |
| `PUID`, `PGID` | — | User and group to run as after mounting. Required when started as root (`mount`). |
| `SMARTCLIPS_UI_LISTEN` | `:8080` | UI listen address (`ui`). |
| `SMARTCLIPS_LOG_LEVEL` | `info` | `debug` for more detail. |

## Security model

- The `mount` service starts as root only to mount the filesystem, then switches every thread to
  `PUID:PGID`. Root stays only as the saved user ID, used again at shutdown to unmount. It needs
  `SYS_ADMIN` (to mount), `SETUID` and `SETGID` (to switch user) and `/dev/fuse`; all other
  capabilities can be dropped, and it needs no network.
- The filesystem is read-only. Inputs must resolve (following symlinks) inside the media folder, and
  clip and folder names can't contain path separators or start with a dot.
- The UI runs as an unprivileged user with no capabilities. Its API is confined to the media folder,
  limits concurrent ffmpeg jobs, refuses cross-site requests and sends a strict Content Security
  Policy. It has no authentication of its own.
- Images are built by GitHub Actions with an SBOM and signed build provenance; see
  [SECURITY.md](SECURITY.md).

## Upgrading from 1.x

Version 2 is a rewrite. The two images (`docker-smartclips`, `docker-smartclips-ui`) are replaced by
one image, `smartclips`, with `mount` and `ui` commands, and:

- The mount point is now a subdirectory: bind the parent with `rshared` and point the media server
  at `<parent>/library`.
- The mount service starts as root and drops to `PUID`/`PGID`; it no longer needs a setuid
  `fusermount3`.
- `clips.json` keeps the same format. Unknown fields are now rejected, and invalid clips are skipped.
- `SMARTCLIPS_CACHE_MAX_MB` is gone: Matroska clips use no cache space, and remuxed clips are kept
  until they're no longer configured.

## Development

See [docs/development.md](docs/development.md).

## License

[GPL-3.0](LICENSE).
