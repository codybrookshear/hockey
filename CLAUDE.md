# hockey: two public rink sites

- `hockey.brookshear.party` (`cmd/schedule`, `internal/schedule`): The Rink Exchange's daily
  schedule from Frontline Connect (HTML, no API; `internal/frontline`), one table per sheet.
- `rhl.brookshear.party` (`cmd/rhl`, `internal/rhl`): the RHL adult league from GameSheet's
  undocumented JSON API (`internal/gamesheet`): a season menu in the header (the current
  season at `/`, earlier ones at `?season=<id>`, carried in every link); a division toggle
  (remembered in a cookie, current season only) filters standings, live, results with
  scorers, upcoming, scoring leaders and goalies; team pages (unfiltered) with the roster and
  its stats; a page per live or final game (`/game/<id>`, what a game's card links to: score
  and shots by period, team stats, goals with assists, penalties, players' lines); logos (server-cached; a team without one borrows the logo of a same-named team
  from another season, else gets its initial, white on red).

Separate apps and containers by design (the user wants them separate sites). Shared:
`internal/cache` (cache + per-source request limits) and `internal/serve` (listen, timeouts,
headers, logging, static). Prefer fewer dependencies; the only one is golang.org/x/net/html.
Phone-first (iPhone mini, 375px): check layouts at that width.

## Architecture (decided)
- Go, server-rendered `net/http` + `html/template`. No JavaScript, no third-party assets in
  pages (logos are proxied), strict CSP. No database, no secrets, no login. Public. One
  cookie: RHL's `division` (an ID; HttpOnly, Lax, Secure when on the socket), set by the
  toggle's `/?division=<id>` links.
- Parse strictly; fail loudly on a changed source (missing fields/columns, wrong date
  heading, bad times) rather than show a wrong or empty table. Tests use saved responses
  (testdata/). Send an honest User-Agent; never impersonate a browser or get past bot
  challenges (GameSheet's pages have one; its API doesn't, today).
- Every request to a source stays bounded however much traffic arrives: ≤20/min per
  source, one at a time, cached (schedule 10 min/day, RHL standings and games 3 min, player
  and goalie stats and rosters 10 min, box scores 30 min or 1 min while live, logos 1 day,
  the league's seasons 6 h). Schedule dates
  limited to today −7…+120 days. Only the league's own seasons can be picked, and rosters
  only for teams, and box scores only for started games, in a season's data. Logos only from URLs in GameSheet's data, on
  imagedelivery.net, raster types, ≤512 KB; no redirects followed.
- GameSheet endpoints used (`gamesheetstats.com/api/`): `leagues/{id}/seasons`,
  `season-info/{id}`, `standings/{season}`, `unified-games/{season}` (paged, has a total),
  `players/standings/{season}` and `goalies/standings/{season}` (paged by limit/offset, no
  total), `teams/{season}/{team}/roster` (players and goalies, before any games too),
  `useBoxScore/getGameStats/{season}/games/{game}`. Found
  in the stats site's JavaScript; player stats and rosters are rendered server-side there,
  so the browser's network tab doesn't show them. A new season's standings have
  `"rank": null` until teams play (some divisions are ranked by hand). Game times come from
  the local `date` and `time`, read in HOCKEY_TZ: GameSheet's `timeStampZulu` follows the
  league's zone there, America/Vancouver, which stays on UTC−7 from Nov 2026, so it's an hour
  early for the Washington rink in winter.
- RHL visitors never wait for GameSheet (decided: the site must load at once). `Server.Run`
  refreshes the current season every 2 min 45 s (player stats every third round; logos and
  the last week's box scores once), or every 15 min after 30 min without a visitor;
  everything else is `cache.Serve`: the copy held, however old, with a newer one fetched
  behind the visitor. Only a key never fetched (an earlier season's first visit) waits.
  Pages are `private, no-cache`, so going back from a game is instant.
- The box score (`useBoxScore/getGameStats/{season}/games/{game}`) is the one endpoint
  without a saved response: the session that added it couldn't reach GameSheet (bot
  challenge on cloud IPs), so its parser and `boxscore-2986401-reconstructed.json` follow
  other projects' notes on the shape. If a real response differs, save one to testdata and
  fix the parser against it. The game page works without it (from the games list: score and
  goals by period, shots, power plays, scorers) and says the scoresheet is missing.
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
