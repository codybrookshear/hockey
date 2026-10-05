# hockey

Two small public sites for The Rink Exchange (Lane County Ice, Eugene), built
to work well on a phone:

- **[hockey.brookshear.party](https://hockey.brookshear.party)**: the rink's
  daily schedule. One table for the Big Sheet, one for the Mini Sheet, with
  times, teams and locker rooms. (`cmd/schedule`, `internal/schedule`)
- **[rhl.brookshear.party](https://rhl.brookshear.party)**: the RHL (adult
  league). Standings by division, results with goal scorers, upcoming games,
  scoring leaders, goalies, and a page per team with its players' stats.
  (`cmd/rhl`, `internal/rhl`)

They're separate apps in separate containers. What they share: the cache and
request limits (`internal/cache`) and server plumbing (`internal/serve`).

## Where the data comes from

Neither source has a public API.

- **Schedule:** the rink's booking site,
  [Frontline Connect](https://www.frontline-connect.com/dailysched.cfm?fac=laneice&facid=1),
  renders its daily schedule as HTML. The app fetches that page (a form POST
  picks the date) and parses its table (`internal/frontline`).
- **RHL:** the league keeps score with [GameSheet](https://gamesheetstats.com).
  Its stats site loads JSON from `gamesheetstats.com/api/…` (the league's
  seasons, standings, games, player and goalie stats), unauthenticated and
  undocumented; the app reads the same (`internal/gamesheet`). The current
  season is the one GameSheet marks active; `RHL_SEASON` pins one instead.

Both are parsed strictly: if a page or response changes shape, the site shows
an error instead of a wrong table. Tests run against saved copies.

Both sites send each source at most 20 requests a minute, one at a time, and
cache answers (schedule days 10 minutes; RHL standings and games 3 minutes,
player stats 10; team logos a day), whatever their own traffic. If a source
is down, the last copy is shown, marked as old.

## Privacy and security

Public: no login, no cookies, no JavaScript, strict CSP. Team logos are
fetched by the server from GameSheet's image CDN and served from the site, so
visitors' browsers only ever talk to these sites. `noindex`, and robots.txt
disallows everything, like the sources' own.

## Running it

```sh
make dev     # schedule on http://<this machine>:8081, RHL on :8082, real data
make test    # offline
```

## Deploying

They share a DigitalOcean droplet with [finance](https://github.com/codybrookshear/finance),
as the Docker Compose project `hockey` (`/opt/hockey`). Each app has its own
network, so it can reach the internet but not the other app or any of
finance's containers. No secrets.

```
Browser → Cloudflare → tunnel → cloudflared (droplet host)
  → /run/hockey-web/web.sock → schedule container
  → /run/rhl-web/web.sock    → rhl container
```

- cloudflared and its config live in the finance repo
  (`deploy/cloudflared/config.yml`). The `hockey.` and `rhl.brookshear.party`
  rules there point at the sockets; they're the only routes without Cloudflare
  Access.
- `scripts/deploy.sh` builds images from the committed HEAD on your
  workstation and loads them on the droplet over SSH (the `finance` alias). No
  registry. `scripts/deploy.sh rhl` (or `schedule`) deploys just one app.

First deploy: `scripts/deploy.sh`, then a DNS record per hostname (CNAME
`hockey` and `rhl` → `<tunnel-id>.cfargotunnel.com`, proxied), then deploy the
tunnel config from the finance repo (`scripts/tunnel-deploy.sh finance`).

Logs: `ssh -t finance 'cd /opt/hockey && sudo docker compose logs --tail 50 rhl'`
(or `schedule`). Rolling back: set `RHL_VERSION` (or `SCHEDULE_VERSION`) in
`/opt/hockey/.env` to the previous version (`sudo docker images 'hockey-*'`),
then `sudo docker compose up -d rhl`.
