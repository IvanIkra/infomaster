#!/bin/sh
# Build the TeX builder's image on the host and start it (D84). Run it once, and again only when texbuild or the fonts
# change: scripts/deploy.sh leaves it alone. TeX itself (infomaster-tex-base) is installed in a container held to one
# CPU at the lowest priority, once; REBASE=1 installs it again. Settings as scripts/deploy.sh, and CMU_FONTS (default
# ~/Library/Fonts): where the CMU Serif .otf faces are.
set -eu
cd "$(dirname "$0")/.."
if [ -f .env ]; then eval "$(grep -E '^DEPLOY_[A-Z]+=[A-Za-z0-9._/-]*$' .env || true)"; fi
host=${DEPLOY_HOST:-server}
dir=${DEPLOY_DIR:-infomaster}
arch=${DEPLOY_ARCH:-amd64}
fonts=${CMU_FONTS:-$HOME/Library/Fonts}
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

(cd go && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags="-s -w" -o "$out/" ./cmd/texbuild)
mkdir "$out/fonts"
for face in cmunrm cmunbx cmunti cmunbi; do cp "$fonts/$face.otf" "$out/fonts/"; done
cp Dockerfile.tex "$out/Dockerfile"
rsync -az --delete "$out/" "$host:$dir/.tex-image/"
rsync -a compose.yaml "$host:$dir/"
packages="texlive-xetex texmf-dist-latexrecommended texmf-dist-latexextra texmf-dist-langcyrillic"
packages="$packages texmf-dist-fontsrecommended fontconfig"
ssh "$host" "cd $dir && set -e
  if [ -n '${REBASE:-}' ] || ! docker image inspect infomaster-tex-base >/dev/null 2>&1; then
    docker rm -f infomaster-tex-install >/dev/null 2>&1 || true
    docker run --name infomaster-tex-install --cpus=1 alpine:3.22 nice -n 19 apk add -q --no-cache $packages
    docker commit -q infomaster-tex-install infomaster-tex-base
    docker rm infomaster-tex-install >/dev/null
  fi
  docker build -q -t infomaster-tex:latest .tex-image
  docker compose up -d --no-build texbuild infomaster && docker compose ps texbuild infomaster"
