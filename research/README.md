# Strategy research harness

Fast, honest replays of trading strategies on a copy of the production database, for trying
ideas before they go into the Go planner. Commodities only (gear sales are too ambiguous to
score: ~30% of gear "sales" reappear at the same price within 2 h).

```sh
python3 -m venv research/.venv && research/.venv/bin/pip install pandas numpy scikit-learn
research/.venv/bin/python research/build.py          # ~45 s: data/ahc.db -> research/cache (read-only)
cd research && .venv/bin/python example.py            # report, stress, a train/test sweep
```

Rebuild the cache whenever you copy a fresh database to `data/ahc.db`. One strategy run takes
0.1–0.5 s, so a sweep of hundreds of parameter sets takes minutes.

## Files

| File | What it is |
|---|---|
| `build.py` | precomputes per house: order book bottoms per scan, every purchase, a features panel |
| `harness.py` | `Market`, `Order`, `Rules`, `run`, `summary`, `report`, `stress`, `sweep` |
| `strategies.py` | `make_dip`, `make_spread`, `make_planner_csv` (replays `ahc backtest -csv`) |
| `example.py` | how to use it |

## Writing a strategy

```python
def make_mine(threshold=0.8):
    def strat(ctx):                 # called at every check; sees only the past
        f = ctx.feat(113)           # features at the latest Horde scan, indexed by item
        b = ctx.book(116)           # bottom of each item's Blackwater book then
        ...
        return [Order(item, buy=113, sell=116, list_price=P, max_price=p, max_units=n)]
    return strat
```

Features (`build.FEATURES`): floor, floor2, listed, n_list, depth105/110/125, paid24_units/n/hours,
med24, vwap24, min24/max24, paid6_units, above_floor24, new24, trend_floor, trend_med (log-slope
per day over 48 h). `ctx.history(house, hours)` gives past rows for your own features or a model
fitted on the training period.

## How positions are scored (`harness._play`)

- Buys real listings at the check, cheapest first, only ones still up at the next scan, never one twice.
- Posts after `post_delay_h` and fills only from later real purchases at >= the list price, each
  purchase used once across all positions, times `share`.
- Relists at `relist_h` under the floor (another deposit); at `window_h` unsold stock is valued
  three ways: `pnl_zero`, `pnl_half` (half the floor), `pnl_floor`. Cash comes back as purchases
  fill, so capital only recycles as fast as things really sell.
- Cuts 5% / 15%, deposits 20% of vendor price (5x on Blackwater), minimum 1s per listing.

`stress()` reruns under adversarial scenarios: half the demand, only certain (partial) fills, 3 h to
post, double deposits, unsold = 0. A strategy worth shipping survives most of them.

## Limits

- ~4.8 days of data: after 24 h warm-up and a 48 h hold only ~2 days of check times remain.
  `sweep` purges (training positions must resolve before the test half opens), which needs a
  short `window_h` (24 h) to leave ~1 day each side. Treat differences under ~10 g/day as noise.
- Deposits: the listing deposit and one relist deposit are reserved from cash when buying; the
  deposit is never refunded on a sale (conservative: the Go planner assumes it comes back).
- Demand is inferred from listings that vanish; trades posted and bought between scans are unseen.
- The market is deflating (Blackwater −19% in 4 days); results drift with it.
- Your own buying doesn't move other buyers; your listings don't change what others post.
