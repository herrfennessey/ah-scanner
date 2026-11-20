# ah-scanner (`ahc`)

WoW Forever auction house collector: one Go binary, one SQLite file, one Docker container on
the NUC. README.md explains what it does, the domain rules (flips, plays, styles, sell-through,
Supply) and how it's deployed; read the relevant section before changing a rule.

## Commands

```sh
go test ./internal/report -run TestBudgetScalesTheFlip -v   # a single test

# Needs BLIZZARD_CLIENT_ID / BLIZZARD_CLIENT_SECRET (cp .envrc.example .envrc; direnv allow)
go run ./cmd/ahc probe -config deploy/config.yaml
go run ./cmd/ahc once  -config deploy/config.yaml
go run ./cmd/ahc serve -config deploy/config.yaml            # http://localhost:4653, /mcp
go run ./cmd/ahc backtest -days 4            # replay and score the flip planner (-queue, -csv)
```

`data/` (local SQLite db, raw bodies) and `.envrc` are gitignored. Go version is in `go.mod`.

## Work locally, on a copy of the database

Make and check changes locally; don't push to `main` to see what something looks like (every push
builds and ships an image). To see a change against real data, put a copy of the production
database at `data/ahc.db` (where to get it is given at query time) and run:

```sh
go run ./cmd/ahc serve -rebuild -config deploy/config.yaml   # http://localhost:4653
```

`.envrc` sets `AHC_DATA_DIR=$PWD/data`, which overrides `data_dir` in the config. `-rebuild`
recomputes the flip cache from the copy with the current code (the collector normally does that
after each scan), so planner and UI changes show up without a collector running. Use
`ahc backtest` on the same copy to score planner changes before shipping them.

`ahc backtest` facts that bite:

- It takes ~20 min for `-days 4` (it rebuilds `BuildFlipInputs` from SQLite at every past scan).
- `-days` counts back from *now*, not from the copy's newest scan, and only plans where every
  hold ends inside the data: on a copy a day old, `-days 4` scores about 2 days.
- It writes to the database it runs on (`BackfillFront`, `ReclassifyGearSales`, a rollup). To
  compare two versions, give each its own copy (`sqlite3 data/ahc.db ".backup other/ahc.db"`, then
  `AHC_DATA_DIR=other`) and start them together so their windows match.
- Its totals score every flip it proposes on its own, with no shared gold: not gold per day.

## Strategy research (`research/`)

A Python harness for trying trading ideas in seconds before touching the Go planner (see
`research/README.md`). `research/.venv/bin/python research/build.py` turns `data/ahc.db` into
`research/cache` (~45 s, read-only); a strategy is a function that sees only the past and returns
orders, and one executor scores everything (shared gold, cuts, deposits, fills only from later real
buyers, relists). `stress()` reruns under adversarial assumptions; `sweep()` picks parameters on
the first half and scores the second. `make_planner_csv` replays `ahc backtest -csv` through the
same executor, so the current planner is the baseline. Commodities only. Confirm a winner with
`ahc backtest` after porting it. The cache and venv are gitignored.

## Architecture

One binary (`cmd/ahc`, subcommands switched in `main.go`), one SQLite file, one Docker container.

**Collection pipeline** (`internal/collector`): `follow.go` learns the second past the hour each
house publishes and polls just after it (or a cron schedule). Each fetch goes
`blizzard` (OAuth + undocumented Forever endpoints, raw zstd bodies kept 48 h) →
`listing` (stream-decodes JSON; fails the whole snapshot if a relied-on field is missing) →
`store.apply` → `tracker` (diffs against the previously open auctions and classifies each change as
sold / cancelled / expired-or-sold / unsold; sold counts are always a `lo–hi` range; the rules are
numbered in `tracker.go` and tabled in README) →
`aggregate` (per-item price stats per snapshot). `Nightly` runs the rollup, retention pruning and
backup.

**Precompute after every publish** (`collector.RefreshAfterPublish`): updates today's `item_daily`
(`rollup`), builds `report.BuildFlipInputs` (the expensive evidence) and stores it as JSON in
`flip_cache`, then rebuilds the Supply page into `supply_cache` / `supply_history`. Readers never
recompute this per request.

**Flip planning is two stages** (`internal/report/flips.go`): `BuildFlipInputs` (heavy, DB, after
each scan) and `PlanFlips` (pure, no DB, per request: applies gold, style and plays). Keep DB access
out of `PlanFlips`; the web handlers, MCP tools and `backtest` all call it on cached or replayed
inputs. `backtest` rebuilds past inputs from auction history and scores plans against buyers who
really paid afterwards.

**Readers**: `internal/web` serves the UI (`static/` embedded via `go:embed`, vanilla JS in
`app.js`), a JSON API and read-only MCP tools (`mcp.go`) from the same server; `internal/report`
also renders the CLI reports. The collector is the only DB writer (WAL mode); everything else reads.

**Houses**: configured by pinned id in `deploy/config.yaml` (113 Horde, 116 Blackwater/Neutral, 110
Alliance view-only). Faction houses take a 5% cut, Blackwater 15% with a 5× deposit. `view_only`
houses are tracked for sales but never traded on; `price_only` skips sale tracking.

Other packages: `stats` (empirical-Bayes shrinkage, Beta intervals via gonum), `supply` (Durotar
Supply and Logistics planner), `version` (build version stamped via `-ldflags`, `dev-<commit>`
locally), `config` (yaml + env secrets).

## Conventions

- Game facts (recipes, drops, rep, coin pouches, crate fills) come from the Blizzard API or cited
  sources, never from memory; leave unknown values unknown (see `internal/supply/data.go`).
- Schema lives in `internal/store/schema.sql` (`CREATE TABLE IF NOT EXISTS`); tests build real
  SQLite stores in temp dirs rather than mocking.
- A new tracker rule only applies to new scans. To apply it to history, extend
  `store.ReclassifyGearSales` (or add a sibling) and bump its `backfill.*` meta key in
  `collector.go`, so the collector reclassifies and rolls up once at its next start.
- Every flip route sizes to the purchases on the house it sells on at or above its price, and
  needs recent ones as proof. The Alliance's sales at home only gate the Horde → Blackwater route;
  sizing it on them left stock unsold.

## Commit messages: Conventional Commits

Every commit on a branch that will merge to `main` uses
[Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/). semantic-release reads
them on each push to `main` (go-semantic-release, `.github/workflows/build.yml`) to pick the
next version, build the image with it, then tag the commit and publish the GitHub release:

| Prefix | Use for | Release |
|---|---|---|
| `feat:` | a new feature or user-visible behaviour | minor (v1.**2**.0) |
| `fix:` | a bug fix | patch (v1.2.**1**) |
| `feat!:` / `fix!:` or a `BREAKING CHANGE:` footer | breaks config, data or the API | major (v**2**.0.0) |
| `perf:` `refactor:` `test:` `docs:` `build:` `ci:` `chore:` `style:` | everything else | none |

A change that should ship as a release (a speed-up users notice, a CI fix that must reach the
image) is `fix:` or `feat:`; only those two types release.

- Format: `type(optional scope): summary`, e.g. `feat(supply): chart gold per Favor over time`.
  Summary in the imperative, lower case after the colon, no trailing period.
- The body explains why. Breaking changes need a `BREAKING CHANGE: <what to do>` footer.
- One logical change per commit, so the type is honest; split a feature and an unrelated fix.
- Only the first line is parsed. Merge commits ("Merge pull request #n") and any other
  non-conventional subject are ignored, so the branch's own commits decide the release: fix a
  wrong message before merging (reword the commit).

## Versions

- Each image is stamped with its version (`internal/version`): the release (`v1.2.3`), or
  `git describe` output for builds without a release (`v1.2.3-4-gabc1234`). Local `go build`
  reports `dev-<commit>`.
- `ahc collect`, `once` and `serve` log `{"msg":"ahc starting","version":…}` first thing,
  `ahc version` prints it, and the Collector page shows the running collector's version. After deploying, check it matches the release
  or commit you expect.
- Stacked PRs: merge the base PR, wait for GitHub to retarget the next one to `main`, then merge
  it. A PR merged into a branch that already merged doesn't reach `main` or the image.

## Before pushing

- `gofmt -l .` prints nothing, `go vet ./...` and `go test ./...` pass.
- `node --check internal/web/static/app.js` for UI changes.
