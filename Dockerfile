FROM golang:1.23-bookworm AS builder

WORKDIR /src

COPY clipfs/go.mod clipfs/go.sum ./
RUN go mod download

COPY clipfs/ .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -o clipfs ./cmd/clipfs


FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y \
    fuse3 \
    ffmpeg \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# FUSE needs this
RUN mkdir -p /dev/fuse

WORKDIR /app

COPY --from=builder /src/clipfs /usr/local/bin/clipfs

# default mount point
RUN mkdir -p /mnt/clipfs

ENTRYPOINT ["clipfs"]