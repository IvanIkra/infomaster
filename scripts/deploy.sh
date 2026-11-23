#!/bin/sh
# Deploy to a small host without compiling there: build the binaries here for the host's platform, send them with the
# web client and Dockerfile.prebuilt, build the image on the host and restart the service. Settings from the
# environment (or .env): DEPLOY_HOST (ssh name, default "server"), DEPLOY_DIR (default "infomaster", relative to the
# remote home), DEPLOY_ARCH (default amd64).
set -eu
cd "$(dirname "$0")/.."
# only the DEPLOY_* lines of .env: the rest are secrets and need not be shell-safe
if [ -f .env ]; then eval "$(grep -E '^DEPLOY_[A-Z]+=[A-Za-z0-9._/-]*$' .env || true)"; fi
host=${DEPLOY_HOST:-server}
dir=${DEPLOY_DIR:-infomaster}
arch=${DEPLOY_ARCH:-amd64}
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

(cd go && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags="-s -w" -o "$out/" ./cmd/infomaster ./cmd/compare)
cp -R go/web/static "$out/static"
cp Dockerfile.prebuilt "$out/Dockerfile"
rsync -az --delete "$out/" "$host:$dir/.image/"
rsync -a compose.yaml tailscale "$host:$dir/"
ssh "$host" "cd $dir && uid=\$(grep '^APP_UID=' .env | cut -d= -f2-) \
  && docker build -q --build-arg APP_UID=\${uid:-1000} -t infomaster:latest .image \
  && docker compose up -d --no-build infomaster && docker compose ps infomaster"
