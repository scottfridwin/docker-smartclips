# One image for both services: `smartclips mount` (FUSE) and `smartclips ui`.
# The build stage runs on the build host and cross-compiles for the target platform.
FROM --platform=$BUILDPLATFORM golang:1.27-trixie@sha256:2f84bc93ecfb2689f782b153fdcd368b5a7ab96c1386c65cdaccf35e726d6a44 AS build
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN GOARM="${TARGETVARIANT#v}" CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/smartclips ./cmd/smartclips

FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f
# ffmpeg: remuxing non-Matroska sources (mount) and thumbnails/previews (ui).
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /mnt/smartclips /cache /data /media
COPY --from=build /out/smartclips /usr/local/bin/smartclips
# The UI runs as this user. The mount service starts as root (user: "0:0") only to mount,
# then switches to PUID:PGID.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/smartclips"]
CMD ["mount"]
HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 CMD ["/usr/local/bin/smartclips", "healthcheck"]
