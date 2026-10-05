#!/usr/bin/env bash
# deploy.sh: deploy the site to the droplet. Run on your workstation:
#
#   scripts/deploy.sh                 # deploys HEAD to the `finance` SSH alias
#
# The droplet is shared with the finance app, and so is its SSH alias, set up
# by that repo (codybrookshear/finance, deploy/cloudflared/README.md). So is
# cloudflared: the hockey.brookshear.party route is in that repo's
# deploy/cloudflared/config.yml.
#
# Builds the image from the committed HEAD (not the files on disk), ships it
# over SSH with the compose file, and runs scripts/deploy-install.sh with sudo
# there. No registry and no secrets.

# $dir (a validated remote temp path) is meant to expand locally, before ssh sends it.
# shellcheck disable=SC2029

set -euo pipefail

DEST="${DEST:-finance}"

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

cd "$(dirname "$0")/.."
# Untracked files are fine: the image comes from `git archive HEAD`.
[[ -z "$(git status --porcelain --untracked-files=no)" ]] || die "commit your changes first: the image is built from HEAD"
version="$(git rev-parse --short=12 HEAD)"
command -v docker >/dev/null || die "docker not found"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
echo "Building hockey:$version..."
git archive --format=tar HEAD |
  docker build -q --label org.opencontainers.image.revision="$(git rev-parse HEAD)" \
    -t "hockey:$version" - >/dev/null
docker save "hockey:$version" | gzip >"$work/image.tar.gz"

dir="$(ssh "$DEST" 'mktemp -d')"
[[ "$dir" =~ ^/tmp/tmp\.[A-Za-z0-9]+$ ]] || die "unexpected remote temp dir: $dir"
echo "Copying to the droplet..."
scp -q "$work/image.tar.gz" deploy/compose.yaml deploy/hockey-web.tmpfiles \
  scripts/deploy-install.sh "$DEST:$dir/"

rc=0
ssh -t "$DEST" "sudo bash $dir/deploy-install.sh $dir $version" || rc=$?
ssh "$DEST" "rm -rf $dir" || true
exit "$rc"
