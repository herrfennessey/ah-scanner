# ah-scanner (`ahc`)

A private price and sell-through history for the WoW Forever **EU PvP Horde and Blackwater**
auction houses, built from Blizzard's API. The web UI opens on an **Advisor** for the questions
you have standing at the AH:

- **Is this a good price?** A verdict for buying and for selling, against 7 days of prices and
  what's listed now.
- **Sell or hold?** For items you have: sell now (with price and time to sell), sell in parts,
  or hold (price unusually low or a supply glut).
- **Cancel or let it ride?** For one of your listings: expected gold either way, deposit rules
  included.
- **Worth flipping?** Buy-and-resell gaps within a house or across Horde and Blackwater.

Plus **Browse** (every item, price history, when-to-post heatmap) and the same answers as MCP
tools for Claude at `/mcp`.

One Go binary, one SQLite file, one Docker container on the NUC.

## How it works

Every 30 minutes the collector downloads both houses (gear and commodities, about 17 MB per
round) with `If-Modified-Since`. Houses refresh about hourly, so roughly half the polls are
free `304 Not Modified` answers. Each new snapshot is diffed against the auctions that were open
before it:

| Change | Classified as | Counts toward |
|---|---|---|
| commodity quantity dropped | sold (certain) | `sold_lo` |
| vanished with ≥ 2 h left, no repost signal | sold | `sold_lo` |
| commodity vanished while cheaper units sat untouched (buys fill cheapest-first) | unsold | neither |
| vanished with time left, but reposted cheaper | cancelled | `sold_hi` only |
| vanished when it could have expired | expired-or-sold | `sold_hi` only |

The API shows listings, not sales, so sold figures are always a range (`est (lo–hi)`), with
`est = lo + 0.25 × ambiguous`. An auction posted and bought between two snapshots is never
seen, so fast movers are undercounted.

**Market value** is the 7-day median of each snapshot's p20: the price at which the cheapest
20% of listed units clear. It ignores both troll listings and single cheap dumps.

## Commands

```
ahc collect                 run forever on the config schedules; serves the web UI and /mcp
ahc once                    one collection cycle + today's rollup
ahc probe [-all]            check credentials and endpoints, save raw samples, print fields seen
ahc rollup [-days N]        recompute daily rows
ahc serve                   web UI + /mcp only
ahc health                  exit 1 if failing or stale (Docker healthcheck)
ahc replay -out x.db        rebuild a database from the raw bodies (48 h) with the current rules

ahc sell "Heavy Quiver"     when and at what price to post
ahc movers -house blackwater
ahc deals -min-discount 0.4
ahc item 4304 -days 30
```

Report flags: `-house horde|blackwater`, `-days N`, `-min-sold N`, `-csv`, `-limit N`.

## Local development

```sh
cp .envrc.example .envrc      # fill in BLIZZARD_CLIENT_ID / BLIZZARD_CLIENT_SECRET, then: direnv allow
go test ./...
go run ./cmd/ahc probe -config deploy/config.yaml
go run ./cmd/ahc once  -config deploy/config.yaml
go run ./cmd/ahc serve -config deploy/config.yaml   # http://localhost:4653
```

`.envrc` and `data/` are gitignored.

## Deploy on the NUC

CI (`.github/workflows/build.yml`) tests and pushes `ghcr.io/herrfennessey/ahc:latest` and
`:sha-<commit>` on every push to `main`. The NUC pulls on a timer; GitHub never reaches the NUC.

One-time setup:

```sh
sudo mkdir -p /opt/ahc/data && sudo chown -R "$USER" /opt/ahc && cd /opt/ahc
# copy deploy/compose.yaml and deploy/config.yaml here
printf 'BLIZZARD_CLIENT_ID=%s\nBLIZZARD_CLIENT_SECRET=%s\nAHC_MCP_TOKEN=%s\n' 'YOUR_ID' 'YOUR_SECRET' "$(openssl rand -hex 24)" > .env
chmod 600 .env
sudo chown 65532 data            # the image runs as distroless nonroot (uid 65532)
# The image is public, so no registry login is needed. If you make it private, log in with a
# classic PAT (read:packages only); GHCR has historically not accepted fine-grained tokens.
#   echo "$GHCR_READ_PAT" | sudo docker login ghcr.io -u herrfennessey --password-stdin
sudo cp deploy/systemd/ahc-update.* /etc/systemd/system/ && sudo systemctl enable --now ahc-update.timer
docker compose up -d
```

- **Deploy now:** `docker compose pull && docker compose up -d`. The timer does this every 15 min.
- **Roll back:** set `IMAGE_TAG=sha-<old commit>` in `.env`, then `docker compose up -d`.
- **Reports:** `docker compose exec ahc /ahc sell "Thick Leather"`.
- **Backups:** `/opt/ahc/data/backup/ahc-YYYYMMDD.db.zst` (3 kept). Add `/opt/ahc/data` to the
  NAS rsync job. To restore: `zstd -d ahc-YYYYMMDD.db.zst -o ahc.db`.

### Schedules

The schedules are standard cron expressions in `config.yaml`, evaluated in Europe/Berlin and
run inside the container (no host cron needed):

```yaml
schedule:
  collect: "*/30 * * * *"   # ± jitter
  nightly: "0 5 * * *"      # rollup, retention, backup
```

A nightly run missed while the container was down is caught up on the next start.

### Health and alerts

`/data/status.json` (also shown at **Collector** in the web UI) records, per feed: the last
success, the last new data, and consecutive failures. `ahc health` and the optional push alert
(`alert.url`: ntfy or a Home Assistant webhook) fire on:

- 3 consecutive failed cycles, or
- a feed whose polls succeed but bring no new data for `stale_after` (3 h). A 304 is normal,
  but an endpoint that answers 304 forever has frozen.

## Connect Claude (MCP)

The container serves MCP over streamable HTTP at `http://<nuc>:4653/mcp` on your local
network. Set `AHC_MCP_TOKEN` so requests need `Authorization: Bearer <token>`.

```sh
claude mcp add --transport http ahc http://<nuc>:4653/mcp \
  --header "Authorization: Bearer $AHC_MCP_TOKEN"
```

Tools (all read-only): `price_check`, `sell_or_hold`, `cancel_or_ride`, `flips`,
`sell_advice` (when to post), `search_items`, `item_summary`, `movers`, `collector_status`.
Claude can read a screenshot of your bags or AH listings and call these for you.

## How the numbers are worked out

Descriptive statistics and one simple stochastic model, chosen for a short, noisy history:

- **Market value:** 7-day median of each scan's p20 (robust to trolls and dumps).
- **Sold per day:** a floor–ceiling range from the rules above; the estimate inside it uses an
  uncalibrated 0.25 weight on ambiguous units.
- **Sell rates by time of day:** shrunk toward the item's overall rate by empirical Bayes (so one
  lucky auction can't make a time slot look great), with an 80% Beta interval covering both
  "did it sell?" and "how many auctions?". Uses gonum.
- **Time to sell, cancel or ride:** buyers arrive as a Poisson process following the item's
  hour-of-day pattern (shrunk toward the house pattern, then toward flat); your place in line
  comes from the order book.
- **Flips:** need at least 5 separate sales seen, ignore the single biggest sale, and resell at
  market value or just under the cheapest competing listing.
- **Thresholds** (hold below 80% of market value, a glut at 1.4× normal supply, and similar) are
  heuristics until a few weeks of outcomes allow backtesting them.

## Storage

SQLite in WAL mode. The collector is the only writer; the web UI, CLI and MCP only read, and
readers never block the writer.

Measured churn is about **50k new auctions an hour** (~1M a day, from one Saturday-morning
hour). Steady state after about 6 months is roughly **7 GB**, plus compressed backups. Check it
against a full week of real data.

| Table | Kept | Approx. size |
|---|---|---|
| `auction` (one row per listing; closures live here) | 30 days | ~4.7 GB |
| `sale_event` (partial commodity buys only) | 30 days | < 0.3 GB |
| `item_snapshot` (per-item price per snapshot) | 30 days full, 6-hourly to 180 days | ~1.8 GB |
| `item_daily` | forever | +0.7 GB/year |

## Known gaps

- **Undocumented API:** the Forever endpoints are community-discovered and may change. `ahc probe`
  shows what the API returns now; raw bodies are kept for 48 h for reprocessing.
- **Calibration:** the 0.25 ambiguous-unit weight and the cancel heuristics still need week-1
  calibration against your own auctions (spec section 8).
- **Deposits:** the deposit rate and the AH cut are assumptions until confirmed in game.
- **Not built yet:** the farming report (`ahc farm`, using Everloot drop rates) is planned next.
