#!/usr/bin/env bash
# deploy.sh: deploy the sites to the droplet. Run on your workstation:
#
#   scripts/deploy.sh                 # both: schedule and rhl
#   scripts/deploy.sh rhl             # just one; the other keeps running as is
#
# The droplet is shared with the finance app, and so is its SSH alias, set up
# by that repo (codybrookshear/finance, deploy/cloudflared/README.md). So is
# cloudflared: the hockey. and rhl.brookshear.party routes are in that repo's
# deploy/cloudflared/config.yml.
#
# Builds the images from the committed HEAD (not the files on disk), ships them
# over SSH with the compose file, and runs scripts/deploy-install.sh with sudo
# there. No registry and no secrets.

# $dir (a validated remote temp path) is meant to expand locally, before ssh sends it.
# shellcheck disable=SC2029

set -euo pipefail

DEST="${DEST:-finance}"
PLATFORM="${PLATFORM:-linux/amd64}" # the droplet's, whatever this machine is
APPS=(schedule rhl)

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

cd "$(dirname "$0")/.."
apps=("$@")
((${#apps[@]} > 0)) || apps=("${APPS[@]}")
for a in "${apps[@]}"; do
  [[ " ${APPS[*]} " == *" $a "* ]] || die "unknown app: $a (apps: ${APPS[*]})"
done
# Untracked files are fine: images come from `git archive HEAD`.
[[ -z "$(git status --porcelain --untracked-files=no)" ]] || die "commit your changes first: images are built from HEAD"
version="$(git rev-parse --short=12 HEAD)"
command -v docker >/dev/null || die "docker not found"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
images=()
for a in "${apps[@]}"; do
  echo "Building hockey-$a:$version..."
  git archive --format=tar HEAD |
    docker build -q --platform "$PLATFORM" --build-arg CMD="$a" \
      --label org.opencontainers.image.revision="$(git rev-parse HEAD)" \
      -t "hockey-$a:$version" - >/dev/null
  images+=("hockey-$a:$version")
done
docker save "${images[@]}" | gzip >"$work/images.tar.gz"

dir="$(ssh "$DEST" 'mktemp -d')"
[[ "$dir" =~ ^/tmp/tmp\.[A-Za-z0-9]+$ ]] || die "unexpected remote temp dir: $dir"
echo "Copying to the droplet..."
scp -q "$work/images.tar.gz" deploy/compose.yaml deploy/hockey-web.tmpfiles deploy/rhl-web.tmpfiles \
  scripts/deploy-install.sh "$DEST:$dir/"

rc=0
ssh -t "$DEST" "sudo bash $dir/deploy-install.sh $dir $version ${apps[*]}" || rc=$?
ssh "$DEST" "rm -rf $dir" || true
exit "$rc"
