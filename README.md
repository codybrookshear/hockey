# hockey

The Rink Exchange's daily schedule (Lane County Ice, Eugene) on a page that
works on a phone: one table for the Big Sheet, one for the Mini Sheet, with
times, teams and locker rooms. Runs at `hockey.brookshear.party`.

## How it works

The rink's booking site, [Frontline Connect](https://www.frontline-connect.com/dailysched.cfm?fac=laneice&facid=1),
has no API. Its daily schedule page is server-rendered HTML, so the site
fetches that page (a form POST picks the date) and parses its table
(`internal/frontline`). If the page changes shape, it shows an error instead
of a wrong schedule.

- Each day is cached for 10 minutes. At most 20 requests a minute go to the
  rink's site, one at a time, whatever the traffic. If the rink's site is down,
  the last copy is shown, marked as old.
- Dates from 7 days back to 120 days ahead.
- Public: no login, no cookies, no JavaScript, strict CSP. `noindex`, and
  robots.txt disallows everything, like the rink's own.

## Running it

```sh
make dev     # http://<this machine>:8081, fetching the real schedule
make test    # offline: saved copies of the rink's page in internal/frontline/testdata
```

## Deploying

It shares a DigitalOcean droplet with [finance](https://github.com/codybrookshear/finance),
in its own Docker Compose project with its own network: it can reach the
internet (the rink's site) but none of finance's containers. It has no secrets.

```
Browser → Cloudflare → tunnel → cloudflared (droplet host)
  → /run/hockey-web/web.sock → hockey container
```

- cloudflared and its config are shared, and live in the finance repo
  (`deploy/cloudflared/config.yml`); the `hockey.brookshear.party` rule there
  points at the socket. It's the only rule without Cloudflare Access.
- `scripts/deploy.sh` builds the image from the committed HEAD on your
  workstation and loads it on the droplet over SSH (the `finance` alias). No
  registry.

First deploy: `scripts/deploy.sh`, then add the hostname's DNS record (CNAME
`hockey` → `<tunnel-id>.cfargotunnel.com`, proxied) and deploy the tunnel
config from the finance repo (`scripts/tunnel-deploy.sh finance`).

Logs: `ssh -t finance 'cd /opt/hockey && sudo docker compose logs --tail 50 web'`.
Rolling back: set `HOCKEY_VERSION` in `/opt/hockey/.env` to the previous
version (`sudo docker images hockey`), then `sudo docker compose up -d web`.
