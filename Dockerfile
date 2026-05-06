FROM golang:1.26-bookworm AS builder

ARG TARGETARCH

WORKDIR /src

COPY clipfs/go.mod clipfs/go.sum ./
RUN go mod download

COPY clipfs/ .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -o clipfs ./cmd/clipfs


FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y \
    fuse3 \
    ffmpeg \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# runtime dirs only (NOT /dev/fuse)
RUN mkdir -p /config /media /mnt/clipfs /cache && \
    chmod 777 /mnt/clipfs /cache

# Allow non-root users to use FUSE with allow_other
RUN echo 'user_allow_other' >> /etc/fuse.conf

WORKDIR /app

COPY --from=builder /src/clipfs /usr/local/bin/clipfs
COPY docker/entrypoint.sh /entrypoint.sh

RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]