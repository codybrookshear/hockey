# hockey: the rink's daily schedule (hockey.brookshear.party)

Personal site: The Rink Exchange's daily schedule (Frontline Connect, no API) scraped
and shown as one table per sheet (Big Sheet, Mini Sheet). Public. Prefer fewer
dependencies; the only one is golang.org/x/net/html.

## Architecture (decided)
- Go, server-rendered `net/http` + `html/template`. No JavaScript, no third-party
  assets, strict CSP. No database, no secrets, no login.
- `internal/frontline`: fetch (POST `SelectedDate=MM/DD/YYYY`) + parse the daily page.
  Fail loudly on a changed layout (missing columns, wrong date heading, bad times)
  rather than show a wrong or empty schedule. Tests use saved pages in testdata/.
- `internal/site`: the page, cache (10 min per day) and the outbound limit (one fetch
  at a time, ≤20/min). Dates limited to today −7…+120 days. Every request we make to
  the rink's site must stay bounded no matter how much traffic the public site gets.
- Shares the droplet `app-01` with codybrookshear/finance, separately: its own Compose
  project (`/opt/hockey`) and network (egress only, no route to finance), Unix socket
  `/run/hockey-web/web.sock`, read-only, no caps, memory/CPU/pids limits.
- cloudflared runs on the host and its config lives in the finance repo
  (`deploy/cloudflared/config.yml`). The hockey rule is the one ingress rule without
  Cloudflare Access, by choice (2026-10-04): the schedule is public anyway.
- Deploy: `scripts/deploy.sh` (workstation) builds from HEAD, ships the image over SSH
  (`finance` alias), runs `scripts/deploy-install.sh` with sudo. No registry.

## Commands
- `make dev` (http://<dev box>:8081, real schedule) / `make test` / `make lint`
