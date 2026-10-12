# Development

## Run the tests

```bash
go test -race ./...
```

The tests use a small committed fixture, `testdata/sample.mkv` (30 s of HEVC with B-frames and a
keyframe every 2 s, AAC audio and SRT subtitles, muxed by ffmpeg). When `ffmpeg` is installed (the
dev container and CI have it), the tests also decode the clips they build and compare their frames
with the source; without it those tests are skipped.

Lint the way CI does:

```bash
golangci-lint run && go vet ./... && gofmt -l .
```

The end-to-end test builds nothing itself: it runs a built image with FUSE and the production
hardening, checks byte-range and remuxed clips against the source, edits clips through the UI,
kills the mount service to test crash recovery and checks the clean shutdown. It needs Docker,
`/dev/fuse`, `jq` and `curl`:

```bash
make e2e        # builds smartclips:dev, then runs scripts/e2e.sh
```

`scripts/make-fixtures.sh` regenerates the fixture with the image's ffmpeg.

## Build

```bash
make build      # bin/smartclips
make image      # smartclips:dev, from build/Dockerfile
```

The image cross-compiles in a build stage, so multi-platform builds (`linux/amd64,linux/arm64`)
only need QEMU for the runtime stage's `apt-get`. go-fuse doesn't compile for 32-bit targets.

## Code layout

| Path | Contents |
| --- | --- |
| `cmd/smartclips` | Entry point: `mount`, `ui` and `healthcheck` commands, config file watching, mounting and the privilege drop |
| `internal/ebml` | Reading and writing EBML, the element framing Matroska uses |
| `internal/mkv` | Parses a Matroska file's header, tracks and cues, and builds a clip layout that references its clusters |
| `internal/layout` | A clip as literal bytes plus byte ranges of a data file; reading, saving and loading |
| `internal/engine` | Turns the clips file into layouts: the persistent index, background builds, the ffmpeg remux fallback, source change detection and `status.json` |
| `internal/clipfs` | The read-only FUSE filesystem |
| `internal/config` | Loading, validating (path confinement, names, metadata) and saving `clips.json` |
| `internal/nfo` | NFO generation |
| `internal/privs` | Dropping from root to `PUID:PGID` |
| `internal/backup` | Optional scheduled copies of `clips.json` (run by the `ui` command) |
| `internal/ui` | The web UI (static files embedded) and its JSON API |

### How a clip is built

1. `mkv.Open` reads the EBML header, `Info`, `Tracks` and `Cues` (found through the `SeekHead`).
2. `BuildClip` picks the last video cue at or before the start and walks the clusters from there to
   the one containing the end, reading only each cluster's header.
3. The clip is a new EBML header and `Segment` with a fresh `SeekHead`, `Info` (duration and title),
   the source `Tracks`, any `Attachments` (referenced) and new `Cues`, followed by the clusters.
   Every cluster gets a new header with its timestamp shifted so the clip starts at 0; because block
   timestamps are relative to their cluster, block data is referenced unchanged. The first cluster
   drops blocks before the keyframe (and open-GOP leading frames, which can't be decoded); if it
   started before the keyframe, its blocks' 2-byte relative timestamps are rewritten. The last
   cluster drops video from the first frame (in decode order) at or after the end, and other
   tracks' blocks at or after the end.
4. The layout is saved in `<cache>/index/<key>.json`. The key covers the source's path, size and
   modification time and the clip's range and name, so a replaced source gets a new layout.

Files that can't be clipped this way (not Matroska, no cues, unknown-size clusters, encrypted
blocks) fall back to `ffmpeg -c copy` into `<cache>/remux/`.

## Continuous integration and releases

All changes reach `main` through a pull request; the **Lint**, **Test** and **E2E** checks must pass
and merges are squashed. Pull requests also build the image for every platform.

- **Every build of `main`** is published as `ghcr.io/scottfridwin/smartclips:main` and
  `:sha-<commit>`, with an SBOM, BuildKit provenance and a GitHub build provenance attestation.
- **Vulnerability scanning**: Trivy scans each new `:main` image and the released `:latest` image
  weekly. Fixable HIGH and CRITICAL findings appear under **Security → Code scanning**; they don't
  block builds.
- **Dependencies**: Go modules, the base images (pinned by digest) and the GitHub Actions (pinned
  to commit SHAs) are updated by [Renovate](https://docs.renovatebot.com/). Updates wait 3 days
  after publication, then merge automatically once the checks pass.
- **Releases** are created automatically when a merge to `main` changes `go.mod`, `go.sum`,
  `build/Dockerfile`, `cmd/` or `internal/`. The version comes from the commit subject: `feat:`
  bumps the minor version, `<type>!:` or a `BREAKING CHANGE` footer bumps the major version, and
  anything else (including Renovate updates) bumps the patch version. A release tags the image that
  was just published as `X.Y.Z`, `X.Y`, `X` and `latest`. To release manually, run the
  **Build, Test & Publish** workflow with a `release_tag` such as `v2.1.0`.
- If an unattended run on `main` fails, an issue titled **CI failed on main** is opened.
