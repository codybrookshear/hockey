# hockey: two public rink sites

- `hockey.brookshear.party` (`cmd/schedule`, `internal/schedule`): The Rink Exchange's daily
  schedule from Frontline Connect (HTML, no API; `internal/frontline`), one table per sheet.
- `rhl.brookshear.party` (`cmd/rhl`, `internal/rhl`): the RHL adult league from GameSheet's
  undocumented JSON API (`internal/gamesheet`): standings, results with scorers, upcoming,
  scoring leaders, goalies, team pages with player stats, logos (server-cached).

Separate apps and containers by design (the user wants them separate sites). Shared:
`internal/cache` (cache + per-source request limits) and `internal/serve` (listen, timeouts,
headers, logging, static). Prefer fewer dependencies; the only one is golang.org/x/net/html.
Phone-first (iPhone mini, 375px): check layouts at that width.

## Architecture (decided)
- Go, server-rendered `net/http` + `html/template`. No JavaScript, no third-party assets in
  pages (logos are proxied), strict CSP. No database, no secrets, no login. Public.
- Parse strictly; fail loudly on a changed source (missing fields/columns, wrong date
  heading, bad times) rather than show a wrong or empty table. Tests use saved responses
  (testdata/). Send an honest User-Agent; never impersonate a browser or get past bot
  challenges (GameSheet's pages have one; its API doesn't, today).
- Every request to a source stays bounded however much traffic arrives: ≤20/min per
  source, one at a time, cached (schedule 10 min/day, RHL standings and games 3 min, player
  and goalie stats 10 min, logos 1 day, current season 6 h). Schedule dates limited to
  today −7…+120 days. Logos only from URLs in GameSheet's data, on imagedelivery.net, raster
  types, ≤512 KB; no redirects followed.
- GameSheet endpoints used (`gamesheetstats.com/api/`): `leagues/{id}/seasons`,
  `season-info/{id}`, `standings/{season}`, `unified-games/{season}` (paged, has a total),
  `players/standings/{season}` and `goalies/standings/{season}` (paged by limit/offset, no
  total). Found in the stats site's JavaScript; player stats are rendered server-side there,
  so the browser's network tab doesn't show them.
- RHL current season: league 620287's seasons, `is_active` first (dates in GameSheet have
  mistakes), else latest started. `RHL_SEASON` pins one.
- Shares the droplet `app-01` with codybrookshear/finance: Compose project `hockey`
  (`/opt/hockey`), a network per app (egress only), sockets `/run/hockey-web/web.sock` and
  `/run/rhl-web/web.sock`, read-only, no caps, memory/CPU/pids limits.
- cloudflared runs on the host; its config lives in the finance repo
  (`deploy/cloudflared/config.yml`). The hockey. and rhl. rules are the only ingress rules
  without Cloudflare Access, by choice (public data).
- Deploy: `scripts/deploy.sh [schedule|rhl]` (workstation) builds from HEAD, ships images
  over SSH (`finance` alias), runs `scripts/deploy-install.sh` with sudo. No registry.

## Commands
- `make dev` (schedule :8081, RHL :8082, real data) / `make test` / `make lint`
