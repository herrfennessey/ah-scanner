"""Example strategies. Each make_*(**params) returns strategy(ctx) -> list[Order].

Copy one and change it: everything a strategy may use is on ctx (see harness.Ctx). Orders are
tried in the order returned, so sort them by what you'd rather fund first.
"""
import numpy as np, pandas as pd
from harness import Order, HOUSE


def make_dip(house=113, alpha=0.8, min_hours=6, trend_min=-0.05, demand_frac=0.5, margin=0.2):
    """Same house: buy a floor that sits well under the median paid, relist at the median paid.
    trend_min drops items whose median paid is falling faster than this per day (log-slope)."""
    def strat(ctx):
        f = ctx.feat(house)
        if f is None: return []
        c = f[(f.floor <= alpha * f.med24) & (f.paid24_hours >= min_hours) & (f.trend_med >= trend_min)]
        out = []
        cut = ctx.cut(house)
        for item, r in c.iterrows():
            P = r.med24
            dep = ctx.deposit_unit(item, house)
            maxp = (P * (1 - cut) - dep) / (1 + margin)
            if r.floor > maxp: continue
            units = int(demand_frac * r.paid24_units)
            exp = (P * (1 - cut) - dep - r.floor) * min(units, r.depth110)
            out.append((exp, Order(item, house, house, P, maxp, units, tag='dip')))
        return [o for _, o in sorted(out, key=lambda x: -x[0])]
    return strat


def make_spread(buy=113, sell=116, cap=110, margin=0.15, min_hours=3, demand_frac=0.25,
                max_gold_g=30, trend_min=-1.0, min_price=0):
    """Cross-house: list on `sell` at min(its floor - 1c, its median paid, the `cap` house's
    median paid) and buy on `buy` whatever is cheap enough to clear `margin` after the cut and
    deposit. Demand: units paid at >= the sell floor over 24 h, times demand_frac."""
    def strat(ctx):
        sf, bf = ctx.feat(sell), ctx.feat(buy)
        if sf is None or bf is None: return []
        j = sf[['floor', 'med24', 'paid24_hours', 'above_floor24', 'trend_med']].join(
            bf[['floor', 'depth110']].rename(columns={'floor': 'bfloor'}), how='inner')
        if cap is not None:
            af = ctx.feat(cap)
            j = j.join(af[['med24']].rename(columns={'med24': 'cap'}), how='inner')
            j = j[j.cap.notna()]
        j = j[(j.paid24_hours >= min_hours) & (j.trend_med.fillna(0) >= trend_min) & j.med24.notna()]
        cut = ctx.cut(sell)
        out = []
        for item, r in j.iterrows():
            P = min(r.floor - 1, r.med24, r.cap if cap is not None else np.inf)
            if P < min_price: continue
            dep = ctx.deposit_unit(item, sell)
            maxp = (P * (1 - cut) - dep) / (1 + margin)
            if r.bfloor > maxp: continue
            units = int(demand_frac * r.above_floor24)
            if units <= 0: continue
            exp = (P * (1 - cut) - dep - r.bfloor) * min(units, max(r.depth110, 1))
            out.append((exp, Order(item, buy, sell, P, maxp, units, max_gold_g * 1e4, tag=f'{buy}->{sell}')))
        return [o for _, o in sorted(out, key=lambda x: -x[0])]
    return strat


def make_planner_csv(path, style='casual'):
    """Replays `ahc backtest -csv` trades (the current planner) through this executor, so it is
    scored by the same rules as everything else. Buys the planner's units up to its cost."""
    df = pd.read_csv(path)
    df = df[df['style'] == style].copy()   # 'kind' is the route (relist/cross); gear simply finds no book
    df['t'] = pd.to_datetime(df['at']).map(lambda x: int(x.timestamp()))
    by_t = {}
    ids = {}
    def item_id(items, name):
        if name not in ids:
            m = items[(items.name == name)]
            ids[name] = int(m.index[0]) if len(m) else None
        return ids[name]
    def strat(ctx):
        if not by_t:   # planner times are fetch times, a few seconds after the scan they planned on
            T = ctx.m.T
            df['scan'] = T[np.searchsorted(T, df.t.values, side='right') - 1]
            by_t.update({t: g.sort_values('expected', ascending=False) for t, g in df.groupby('scan')})
        g = by_t.get(ctx.t)
        if g is None: return []
        out = []
        for r in g.itertuples():
            iid = item_id(ctx.items, r.item)
            if iid is None: continue
            out.append(Order(iid, HOUSE[r.buy_house], HOUSE[r.sell_house], r.sell_at, np.inf, r.units, r.cost * 1.02, tag='planner'))
        return out
    return strat
