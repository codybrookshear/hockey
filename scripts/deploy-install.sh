#!/usr/bin/env bash
# deploy-install.sh: install or update the sites on the droplet. Run by
# scripts/deploy.sh with sudo; SRC is the temp dir it filled:
#   images.tar.gz, compose.yaml, hockey-web.tmpfiles, rhl-web.tmpfiles
#
#   deploy-install.sh <dir> <version> <app>...    apps: schedule, rhl
#
# Layout:
#   /opt/hockey          compose.yaml and .env (Compose project "hockey"; .env
#                        holds each app's version, so one can be updated alone)
#   /run/hockey-web      the schedule's socket, which cloudflared connects to
#   /run/rhl-web         the RHL site's socket

set -euo pipefail

SRC="${1:?usage: deploy-install.sh <dir> <version> <app>...}"
VERSION="${2:?usage: deploy-install.sh <dir> <version> <app>...}"
shift 2
APP=/opt/hockey

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root"
[[ "$VERSION" =~ ^[0-9a-f]{7,40}$ ]] || die "bad version: $VERSION"
(($# > 0)) || die "no apps given"
command -v docker >/dev/null || die "docker isn't installed"

declare -A socket=([schedule]=/run/hockey-web/web.sock [rhl]=/run/rhl-web/web.sock)
declare -A var=([schedule]=SCHEDULE_VERSION [rhl]=RHL_VERSION)
for a in "$@"; do [[ -n "${socket[$a]:-}" ]] || die "unknown app: $a"; done

echo "Loading images..."
gunzip -c "$SRC/images.tar.gz" | docker load -q
# Before anything changes: an image built for another platform (an arm64 Mac's,
# say) would replace the running site with one that can't start.
arch="$(docker version --format '{{.Server.Arch}}')"
for a in "$@"; do
  got="$(docker image inspect --format '{{.Architecture}}' "hockey-$a:$VERSION")"
  [[ "$got" == "$arch" ]] || die "hockey-$a:$VERSION is built for $got, but this machine is $arch"
done

# Versions: the new one for the apps being deployed; the others keep theirs.
declare -A ver=()
if [[ -f "$APP/.env" ]]; then
  while IFS='=' read -r k v; do
    for a in "${!var[@]}"; do [[ "$k" == "${var[$a]}" ]] && ver[$a]="$v"; done
  done <"$APP/.env"
fi
for a in "$@"; do ver[$a]="$VERSION"; done
for a in "${!var[@]}"; do
  [[ "${ver[$a]:-}" =~ ^[0-9a-f]{7,40}$ ]] || die "no version of $a installed yet: deploy it too the first time"
done

install -d -m 0755 "$APP"
install -m 0644 "$SRC/compose.yaml" "$APP/"
cat >"$APP/.env" <<ENV
COMPOSE_PROJECT_NAME=hockey
SCHEDULE_VERSION=${ver[schedule]}
RHL_VERSION=${ver[rhl]}
ENV
chmod 0644 "$APP/.env"

# Socket directories, now and at every boot.
install -m 0644 "$SRC/hockey-web.tmpfiles" /etc/tmpfiles.d/hockey-web.conf
install -m 0644 "$SRC/rhl-web.tmpfiles" /etc/tmpfiles.d/rhl-web.conf
systemd-tmpfiles --create /etc/tmpfiles.d/hockey-web.conf /etc/tmpfiles.d/rhl-web.conf

cd "$APP"
docker compose config --quiet
echo "Starting $*..."
# --remove-orphans: the first layout's single "web" service, if it's still there.
docker compose up --detach --remove-orphans "$@"

for a in "$@"; do
  sock="${socket[$a]}"
  # robots.txt doesn't touch the sources: proof the server is up.
  code=""
  for _ in $(seq 1 20); do
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 --unix-socket "$sock" \
      http://localhost/robots.txt || true)"
    [[ "$code" == 200 ]] && break
    sleep 1
  done
  if [[ "$code" != 200 ]]; then
    docker compose logs --tail 30 "$a"
    die "$a isn't answering (got '$code', want 200); see above"
  fi
  # The front page does: proof it can read its source. The source being down
  # isn't a reason to fail the deploy.
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 30 --unix-socket "$sock" http://localhost/ || true)"
  if [[ "$code" == 200 ]]; then
    echo "$a is up on $sock and read its source."
  else
    echo "WARNING: $a is up, but its front page answered '$code'. Is its source down?"
    echo "  cd $APP && sudo docker compose logs --tail 20 $a"
  fi
done

# Keep each app's current image and the one before, for rolling back.
for a in "${!var[@]}"; do
  repo="hockey-$a"
  keep="$(docker images --format '{{.Tag}}' "$repo" | grep -vx "${ver[$a]}" | head -1 || true)"
  { docker images --format '{{.Repository}}:{{.Tag}}' "$repo" |
      grep -v -e ":${ver[$a]}\$" ${keep:+-e ":$keep\$"} | xargs -r docker rmi -f >/dev/null; } || true
done
# The first layout's image, "hockey", if it's still around.
docker images -q hockey | xargs -r docker rmi -f >/dev/null 2>&1 || true

echo
echo "Deployed $VERSION: $*."
