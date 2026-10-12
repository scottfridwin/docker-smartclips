# Security policy

## Supported versions

Only the latest release receives fixes. Images are rebuilt automatically when the base image or
dependencies get security updates.

## Reporting a vulnerability

Please report vulnerabilities privately through [GitHub's private vulnerability reporting](https://github.com/scottfridwin/docker-smartclips/security/advisories/new)
rather than in a public issue. Include the image version, how to reproduce the problem and its
impact. You should get a response within a week.

Areas of particular interest: escaping the media folder (through clip inputs, names, symlinks or the
UI API), the privilege drop in the `mount` service, and the UI's request handling.

## Verifying images

Every published image has a build provenance attestation signed by GitHub Actions:

```bash
gh attestation verify oci://ghcr.io/scottfridwin/smartclips:2 --repo scottfridwin/docker-smartclips
```
