# ah-scanner (`ahc`)

A private price and sell-through history for the WoW Forever **EU PvP Horde and Blackwater**
auction houses, built from Blizzard's API. The web UI opens on **Flips**: trades sized to the
gold you have (a slider, default 40g) and how often you trade (casual by default), most expected
profit first.

| Column | Meaning |
|--------|---------|
| Buy from, Sell at | the houses: Horde, or Neutral (Blackwater) |
| Buy at | how many to buy, up to what price each |
| List at | the price to post at, and the listing length |
| Profit | expected profit (below) |
| Play | what to do once it's posted: hold, check in or camp |
| Cancel if position > | check in: relist under the cheapest when more units than this are listed cheaper than yours; camp: when undercut; hold: never |

Three routes: buy underpriced listings and relist them in the same house; buy on Neutral and
sell on Horde; or buy on Horde and sell on Neutral to Alliance players, for less than they pay
at home (only for items the Alliance buys several times a day). On Neutral, the resale is never
above the dearer of the Horde and Alliance prices: every buyer there can buy at home instead,
nor above what buyers on Neutral lately paid. Like the other routes it is sized to the purchases
on Neutral at or above its price and needs 2 of them in the last 12 hours: the Alliance's demand
at home only says how many might come.

### Styles and plays

| Play | What you do | Listing | Window |
|------|-------------|---------|--------|
| **hold** | post and leave it, even when undercut: commodities sell the cheaper listings first, then yours | 24h | 1 day |
| **check in** | look every ~5 h; relist when the queue cheaper than yours is longer than buyers take before your listing runs out | 24h | 1 day |
| **camp** | check hourly, relist whenever undercut; the only play for swinging or sliding prices | 8h | 12 h |

The check-in queue is the units buyers take a day near the floor times the hours left on the
listing (19 at the first visit): `RelistQueue` in `internal/report/flips.go`. The page shows it
for the first visit; it shrinks as the listing runs down.

| Style | Visits | Plays | Gold per flip | Min return | Sells on |
|-------|--------|-------|---------------|------------|----------|
| active | about hourly | hold, check in, camp | 50% | 20% | Horde, Neutral |
| casual (default) | 4–5 times a day | hold, check in | 40% | 20% | Horde, Neutral |
| away | once or twice a day | hold | 30% | 25% | Horde only |

Each item is planned with every play the style allows and the one with the most expected profit
is kept (a camp's counted at 80%: that's what camping delivered in the backtest, against ~99% for
holds), so active always sees at least what casual does, and casual what away does (a test
checks this over random markets). Old names still work: `regular` and the risk levels
`high`/`medium`/`low` map to active, casual and away.

### How a flip is judged

On what buyers do, not what sellers ask:

- The resale is capped at the **median price paid** over the last 24 hours, so a market value
  left over from before a crash can't make a dump look like a bargain.
- At least **2 purchases at or above the resale price** in the last 12 hours. The amount bought
  must fit the purchases at or above that price over the last 2 days, and your units plus
  everything listed within 10% of your price must sell within the play's window.
- **Sliding markets:** an asking price falling over 10% a day (the median ask, by Theil-Sen, so
  one dump can't trigger it), or a resale more than 15% under market value, is left to camping.
- **Sell chance** and **undercut chance** are measured per item from listings that were the
  cheapest when posted (`auction.front`): the share that sold within one round of the play (a
  hold's 24 hours, or until the next visit), and the share that a cheaper listing undercut in
  that time while still up. Items with few such listings lean on the house average.

**Expected profit** weighs the play's rounds by those chances. Looking is free: a round that
doesn't sell costs a relist (the deposit, and a typical undercut of about 3%) only when it was
undercut, or the deposit when the listing ran out; holds never relist. Stock still unsold at the
end of the window is valued at what buyers are paying then. It's never shown above the profit if
everything sells (which counts one lost deposit). Deposits are 20% of the vendor price with a 1s
minimum per auction (confirmed against Journalator posting logs), 5× on Neutral
(`deposit_factor`); the cut is 5% on faction houses and 15% on Neutral.

Flips are ranked by expected profit, not profit per day: gold sitting idle isn't the limit, and
the chance it sells is already weighed in. **Going fast** marks underpriced listings of the kind
most likely to be gone within the hour (about half are). The page updates itself after every
scan: new flips get a badge, a toast appears and the tab title shows the count.

What a scan can see: a commodity listing that loses units, or vanishes with nothing cheaper
left, was bought (commodities fill cheapest first). Vanished gear may have been cancelled, so it
counts only if no cheaper copy outlasted it and no identical copy at the same price appeared in
its place. Anything listed and bought within one scan interval is never seen, so demand is, if
anything, undercounted.

### Backtest and local preview

`ahc backtest -days 4` replays the planner at every past scan, with the inputs as they were then,
and scores each flip per style and play: the resale fills only from buyers who really paid at
least its price afterwards, and is played as planned (a hold left alone; a check-in relisted under
the cheapest when the recorded queue cheaper than it is too long; a camp relisted whenever
undercut; never under cost). Unsold stock is valued at the floor when the window ends. `-queue 0.5`
or `-queue 2` scales the check-in queue to test the rule. `-csv` lists every trade.

To look at the UI without deploying, run it on a copy of the database: point a config's
`data_dir` at the copy and run `ahc serve -rebuild`. `-rebuild` recomputes the flips from the
copy first (the collector normally does that after each scan), using the houses stored in it.

Each item page has a **price distribution**: units listed at each price above the axis, units
bought over the last 48 hours below it, market value and the median paid marked, and the cost of
buying everything up to any price on hover.

**Upgrades** (preview, druids for now) loads a character from the armory (first and last name,
e.g. Sine Wave) and ranks every item listed now on Horde and Blackwater that beats what it
wears, by stat gain per gold (or, with **Buy once**, biggest gain first and nothing more than 10
levels below). Pick a **spec** and the **content** (questing, dungeon, boss); the weights are
worked out per character from its armory sheet and its real talents (both dual-spec groups come
back from the armory, whichever is active), and the page explains each one:

- **Feral (cat)**, in attack power: a steady-state cat DPS model (`internal/upgrade/feral.go`:
  energy, combo points, Omen of Clarity, Rake/Rip/Bite, the attack table by level difference),
  weights as finite differences. Ability ranks are from `wowsims/forever`; Natural Instinct's
  crit bonus and Ferocity on Rake follow their tooltips. Stamina is your own choice (1 by default).
- **Restoration**, in healing power: one mana bar per fight, blending throughput and
  mana-limited healing by how often you run out (`internal/upgrade/resto.go`: Reflection, Bloom,
  Innervate, talents by level). Base mana matches the client's `basemp` table.

Both are pinned to their specs' test vectors. Weights stay editable on the page (kept in the
browser). Procs, use and equip effects are flagged, not valued. A new spec is a preset in
`internal/upgrade` plus its model; the page lists specs from `/api/upgrades/specs`.

Random-suffix stats are computed from the client tables (`internal/upgrade/data.go`, `go
generate`): the bonus list's share of each stat times `RandPropPoints` for the item level,
quality and the slot's column. The slot columns were solved from 663 fixed-stat items whose
Blizzard stats one column reproduces for 100% of each slot (`TestColumnsReproduceBlizzardStats`);
each loaded character's suffix gear is checked against them too. Trinkets, idols, thrown and
off-hand weapons had no samples and show a range (≈).

**Supply** plans Durotar Supply and Logistics hand-ins at Horde prices:

- **Crates for Merchant's Favor**, cheapest first: an empty Waylaid Crate plus its cheapest
  fill bundle, divided by the Favor the sealed crate pays (raw materials 5/10/15 by tier,
  crafted double; Artisan unknown). Fill lists come from each crate's in-game tooltip (API).
- **Writs for reputation**, by net gold: the writ's coin pouch minus the writ and the item it
  asks for. Rep per writ comes from the quest API (75/125/200). Each row names the customer
  and zone; filter by world, continent or zone, with a per-zone summary. The Forged Delivery
  Receipt (15 Favor + an empty crate → 50 rep) is shown as a benchmark, and sealed writs are
  valued at their tier's average order.
- **Is now a good time?** Two charts over the last 2, 7 or 30 days: the cheapest crate's gold
  per Favor and the cheapest writ's gold per rep (with the Delivery Receipt), one point per
  Horde scan. Each says where now sits against the period (good when cheaper than 75% of it).
  The first run backfills the charts by rebuilding each past scan's order book from the auction
  history (as far back as `auction_retention_days`).
- The page is computed once after each Horde scan (`supply_cache`, `supply_history`), not per
  request.
- Coin pouch amounts and customers come from community guides (sources in
  `internal/supply/data.go`); override the pouch in `config.yaml` under `supply.coin_pouch`.

Hovering any item icon shows its in-game tooltip, from the item API, with this house's prices.

Item pages (and the hover tooltip) carry a **hold or chase** hint, judged against the item's own
7-day history on that house (`internal/report/hint.go`):

| Hint | When | Backtest, Horde (Blackwater) |
|---|---|---|
| Sell now | priced ≥1.25× its usual level | ≥1.5×: 87% (69%) fell 10%+ within 12 h; 1.25–1.5×: 68% (53%) |
| Chase it | price down 7%+ in ~3 h while listings are 1.3× usual | 59% (50%) fell another 10%+ |
| Slow | what's listed takes 3+ days of sales to clear | — (each cancel loses a deposit) |
| Hold | steady at its usual price | 71% (58%) stayed within 10% |

The odds come from about two days of history; rerun the backtest as it grows.

Plus **Browse** (every item, price history, when-to-post heatmap) and the same data as MCP
tools for Claude at `/mcp`.

One Go binary, one SQLite file, one Docker container on the NUC.

## Versions and releases

Commits follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) (see
`AGENTS.md`). Each push to `main` works out the next version with go-semantic-release (`feat` →
minor, `fix` → patch, breaking → major), builds and pushes the image, then tags the commit and
publishes a GitHub release. Every image is pushed as `latest` and `sha-<commit>`; a release's
image also as `v<version>`, which you can pin with `IMAGE_TAG=v1.2.3` in the NUC's `.env`.
Pushes with nothing to release are built as `git describe` versions (`v1.2.3-4-gabc1234`).

The running build logs `ahc starting` with its version, `ahc version` prints it, and the
Collector page shows it.

## How it works

Blizzard publishes each house once an hour at a stable second past the hour (about :18:50).
In `follow` mode (the default) the collector learns that time per house and polls 15 s after
it, retrying every 30 s until the new data appears, so each update is seen within about a
minute. Polls use `If-Modified-Since`, so a check before the data changes is a free
`304 Not Modified`. Each new snapshot is diffed against the auctions that were open
before it:

| Change | Classified as | Counts toward |
|---|---|---|
| commodity quantity dropped | sold (certain) | `sold_lo` |
| vanished with ≥ 2 h left, no repost signal | sold | `sold_lo` |
| commodity vanished while cheaper units sat untouched (buys fill cheapest-first) | unsold | neither |
| vanished with time left, but reposted cheaper (gear: or at the same price) | cancelled | `sold_hi` only |
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
ahc serve [-rebuild]        web UI + /mcp only; -rebuild recomputes the flips first (a db copy)
ahc health                  exit 1 if failing or stale (Docker healthcheck)
ahc replay -out x.db        rebuild a database from the raw bodies (48 h) with the current rules

ahc sell "Heavy Quiver"     when and at what price to post
ahc movers -house blackwater
ahc deals -min-discount 0.4
ahc item 4304 -days 30
ahc backtest -days 3        replay the flip planner and score it (-gold, -every, -queue, -csv)
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

Schedules live in `config.yaml` and run inside the container (no host cron needed):

```yaml
schedule:
  collect: follow           # poll each house just after it publishes; or a cron expression
  nightly: "0 5 * * *"      # rollup, retention, backup (cron, Europe/Berlin)
```

The Collector page shows how many seconds after each publish the data was fetched.

**After editing `config.yaml`, run `docker compose up -d --force-recreate`, not `restart`.**
The file is mounted individually, and most editors (and `sed -i`) replace the file, which a
running container doesn't see.

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

Tools (all read-only): `flips` (with your gold and trading style), `sell_advice` (when to post an item),
`search_items`, `item_summary`, `movers`, `collector_status`.

## How the numbers are worked out

Descriptive statistics and one simple stochastic model, chosen for a short, noisy history:

- **Market value:** 7-day median of each scan's p20 (robust to trolls and dumps).
- **Sold per day:** a floor–ceiling range from the rules above; the estimate inside it uses an
  uncalibrated 0.25 weight on ambiguous units.
- **Sell rates by time of day:** shrunk toward the item's overall rate by empirical Bayes (so one
  lucky auction can't make a time slot look great), with an 80% Beta interval covering both
  "did it sell?" and "how many auctions?". Uses gonum.
- **Flips:** sales speed comes from daily counts of separate confirmed sales, with the single
  largest sale removed (one bulk buyer or a misread cancel can't make a market look liquid),
  and is capped by the recent purchases at or above the resale price. The expensive part,
  including the evidence (recent purchase ladders in 3% price steps, the median paid, the trend
  and the cheapest-when-posted sell rates), is computed right after each publish; your gold and
  style are applied per request.
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
| Alliance auctions (view-only, tracked for its sales) | 30 days | ~4–5 GB more |

## Known gaps

- **Undocumented API:** the Forever endpoints are community-discovered and may change. `ahc probe`
  shows what the API returns now; raw bodies are kept for 48 h for reprocessing.
- **Calibration:** the 0.25 ambiguous-unit weight and the cancel heuristics still need week-1
  calibration against your own auctions (spec section 8).
- **Deposits:** the deposit rate and the AH cut are assumptions until confirmed in game.
- **Not built yet:** the farming report (`ahc farm`, using Everloot drop rates) is planned next.
