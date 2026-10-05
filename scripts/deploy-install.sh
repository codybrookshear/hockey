#!/usr/bin/env bash
# deploy-install.sh: install or update the site on the droplet. Run by
# scripts/deploy.sh with sudo; SRC is the temp dir it filled:
#   image.tar.gz, compose.yaml, hockey-web.tmpfiles
#
# Layout:
#   /opt/hockey          compose.yaml and .env (Compose project "hockey")
#   /run/hockey-web      the site's socket, which cloudflared connects to

set -euo pipefail

SRC="${1:?usage: deploy-install.sh <dir> <version>}"
VERSION="${2:?usage: deploy-install.sh <dir> <version>}"
APP=/opt/hockey
SOCKET=/run/hockey-web/web.sock

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root"
[[ "$VERSION" =~ ^[0-9a-f]{7,40}$ ]] || die "bad version: $VERSION"
command -v docker >/dev/null || die "docker isn't installed"

echo "Loading the image..."
gunzip -c "$SRC/image.tar.gz" | docker load -q

install -d -m 0755 "$APP"
install -m 0644 "$SRC/compose.yaml" "$APP/"
cat >"$APP/.env" <<ENV
COMPOSE_PROJECT_NAME=hockey
HOCKEY_VERSION=$VERSION
ENV
chmod 0644 "$APP/.env"

# The socket directory, now and at every boot.
install -m 0644 "$SRC/hockey-web.tmpfiles" /etc/tmpfiles.d/hockey-web.conf
systemd-tmpfiles --create /etc/tmpfiles.d/hockey-web.conf

cd "$APP"
docker compose config --quiet
echo "Starting the site..."
docker compose up --detach web

# robots.txt doesn't touch the rink's site: proof the server is up.
code=""
for _ in $(seq 1 20); do
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 --unix-socket "$SOCKET" \
    http://localhost/robots.txt || true)"
  [[ "$code" == 200 ]] && break
  sleep 1
done
if [[ "$code" != 200 ]]; then
  docker compose logs --tail 30 web
  die "the site isn't answering (got '$code', want 200); see above"
fi
# Today's page does: proof it can read the schedule. The rink's site being
# down isn't a reason to fail the deploy.
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 --unix-socket "$SOCKET" http://localhost/ || true)"
if [[ "$code" == 200 ]]; then
  echo "The site is up on $SOCKET and read today's schedule."
else
  echo "WARNING: the site is up, but today's page answered '$code'. Is the rink's site down?"
  echo "  cd $APP && sudo docker compose logs --tail 20 web"
fi

# Keep the current image and the previous version's, for rolling back.
keep="$(docker images --format '{{.Tag}}' hockey | grep -vx "$VERSION" | head -1 || true)"
{ docker images --format '{{.Repository}}:{{.Tag}}' hockey |
    grep -v -e ":$VERSION\$" ${keep:+-e ":$keep\$"} | xargs -r docker rmi -f >/dev/null; } || true

echo
echo "Deployed $VERSION."
