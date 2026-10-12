# Dev container for smartclips: Go, golangci-lint, ffmpeg (tests and remuxing) and fuse3.
FROM mcr.microsoft.com/devcontainers/go:1.27-bookworm

# Drop the upstream yarn apt source: its signing key has expired and breaks `apt-get update`.
RUN find /etc/apt -type f \( -name '*.list' -o -name '*.sources' \) \
      -exec grep -lE 'dl\.yarnpkg\.com' {} + 2>/dev/null | xargs -r rm -f || true

RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg fuse3 jq \
 && rm -rf /var/lib/apt/lists/*

# Keep in step with GOLANGCI_LINT_VERSION in .github/workflows/build.yml.
ARG GOLANGCI_LINT_VERSION=v2.14.0
RUN curl -fsSL "https://raw.githubusercontent.com/golangci/golangci-lint/${GOLANGCI_LINT_VERSION}/install.sh" \
      | sh -s -- -b /usr/local/bin "${GOLANGCI_LINT_VERSION}"
