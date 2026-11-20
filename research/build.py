"""Precompute the research cache from the SQLite database (read-only).

    python research/build.py [-db data/ahc.db] [-out research/cache]

Per house (commodities only), writes <out>/<house>.pkl holding:
  T      snapshot times (last_modified), sorted
  book   {t: DataFrame[item, price, qty, aid, last_seen]} the bottom of each item's book at t
         (up to BOOK_RANK cheapest listings, at most BOOK_MULT x the floor), qty = units left at t
  feat   DataFrame indexed (t, item): what a strategy may know at t (past data only, see FEATURES)
  fills  DataFrame[item, ts, price, units, aid, certain] every purchase seen, sorted by (item, ts)
plus <out>/items.pkl (id, name, quality, class, sell_price, ...).

A purchase is a partial buy (units left on an auction dropped: certain) or a whole auction
that closed as 'sold' (tracker rule 5: vanished with time left, no cheaper untouched units:
certain=False here so stress tests can drop them).
"""
import argparse, os, pickle, sqlite3, time
import numpy as np, pandas as pd

H = 3600
BOOK_RANK = 40
BOOK_MULT = 3.0
FEATURES = """floor, floor2 (next listing's price), listed (units), n_list, depth105/110/125 (units at or
below floor x1.05/1.10/1.25), paid24_units, paid24_n (purchases), paid24_hours (distinct scans with a
purchase), med24 (unit-weighted median paid), vwap24, min24/max24 paid, paid6_units, above_floor24
(units paid at >= this floor), new24 (units newly listed), trend_floor / trend_med (log-slope per
day over the last 48 h)"""


def wmedian_by_item(df):
    """Unit-weighted median price per item; df sorted by (item, price)."""
    cu = df.groupby('item').units.cumsum().values
    tot = df.groupby('item').units.transform('sum').values
    ok = cu >= tot / 2
    first = pd.Series(ok, index=df.index).groupby(df.item.values).idxmax()
    return df.loc[first.values].set_index('item').price


def build_house(db, h):
    t0 = time.time()
    a = pd.read_sql(f"""select auction_id aid, item_id item, unit_price price, qty_first, qty_last,
        first_seen, last_seen, closed_at, outcome from auction
        where house_id={h} and kind='commodity' and unit_price>0""", db)
    se = pd.read_sql(f"""select auction_id aid, item_id item, ts, units, unit_price price
        from sale_event where house_id={h} and kind='commodity'""", db)
    T = np.array(sorted(pd.read_sql(f"select distinct last_modified t from snapshot where house_id={h} and kind='commodity'", db).t))
    full = a[a.outcome == 'sold'][['item', 'closed_at', 'price', 'qty_last', 'aid']].rename(columns={'closed_at': 'ts', 'qty_last': 'units'})
    full['certain'] = False
    part = se[['item', 'ts', 'price', 'units', 'aid']].copy(); part['certain'] = True
    fills = pd.concat([full, part], ignore_index=True)
    fills = fills[fills.units > 0].sort_values(['item', 'ts', 'price'], kind='stable').reset_index(drop=True)

    se_sorted = se.sort_values('ts')
    fp = fills.sort_values(['ts']).reset_index(drop=True)
    book, rows = {}, []
    for t in T:
        op = a[(a.first_seen <= t) & (a.last_seen >= t)][['item', 'price', 'qty_first', 'aid', 'last_seen', 'first_seen']]
        sold_before = se_sorted[se_sorted.ts <= t].groupby('aid').units.sum()
        qty = op.qty_first.values - sold_before.reindex(op.aid.values).fillna(0).values
        op = op.assign(qty=qty)
        op = op[op.qty > 0].sort_values(['item', 'price'], kind='stable')
        g = op.groupby('item')
        floor = g.price.min()
        f = pd.DataFrame({'floor': floor, 'listed': g.qty.sum(), 'n_list': g.size()})
        fl = op.item.map(floor).values
        for m, name in ((1.05, 'depth105'), (1.10, 'depth110'), (1.25, 'depth125')):
            f[name] = op[op.price.values <= fl * m].groupby('item').qty.sum()
        rank = g.cumcount().values
        f['floor2'] = op[rank == 1].set_index('item').price
        keep = (rank < BOOK_RANK) & (op.price.values <= fl * BOOK_MULT)
        book[t] = op[keep][['item', 'price', 'qty', 'aid', 'last_seen']].reset_index(drop=True)
        # what was bought over the past 24 h / 6 h
        w = fills[(fills.ts >= t - 24 * H) & (fills.ts < t)]
        if len(w):
            ws = w.sort_values(['item', 'price'], kind='stable')
            gw = ws.groupby('item')
            f['paid24_units'] = gw.units.sum()
            f['paid24_n'] = gw.size()
            f['paid24_hours'] = gw.ts.nunique()
            f['vwap24'] = (ws.price * ws.units).groupby(ws.item).sum() / f['paid24_units']
            f['min24'] = gw.price.min(); f['max24'] = gw.price.max()
            med = wmedian_by_item(ws)
            f['med24'] = med
            w6 = w[w.ts >= t - 6 * H]
            f['paid6_units'] = w6.groupby('item').units.sum()
            af = ws[ws.price.values >= ws.item.map(floor).fillna(np.inf).values]
            f['above_floor24'] = af.groupby('item').units.sum()
        new = a[(a.first_seen > t - 24 * H) & (a.first_seen <= t)]
        f['new24'] = new.groupby('item').qty_first.sum()
        f['t'] = t
        rows.append(f.reset_index().rename(columns={'index': 'item'}))
    feat = pd.concat(rows, ignore_index=True)
    for c in ('depth105', 'depth110', 'depth125', 'paid24_units', 'paid24_n', 'paid24_hours', 'paid6_units', 'above_floor24', 'new24', 'listed', 'n_list'):
        if c in feat: feat[c] = feat[c].fillna(0)
    feat = feat.sort_values(['item', 't']).reset_index(drop=True)
    # trends: log-slope per day over the last 48 h of scans (rolling cov/var on time)
    x = (feat.t - T[0]) / 86400.0
    for col, name in (('floor', 'trend_floor'), ('med24', 'trend_med')):
        y = np.log(feat[col].astype(float))
        xm = x.where(y.notna())
        d = pd.DataFrame({'item': feat.item, 'x': xm, 'y': y, 'xy': xm * y, 'xx': xm * xm})
        r = d.groupby('item')[['x', 'y', 'xy', 'xx']].rolling(48, min_periods=12).mean().reset_index(level=0, drop=True)
        feat[name] = ((r.xy - r.x * r.y) / (r.xx - r.x * r.x)).reindex(feat.index)
    feat = feat.set_index(['t', 'item']).sort_index()
    print(f'house {h}: {len(T)} scans, {len(fills)} purchases, {len(feat)} feature rows, {time.time()-t0:.0f}s', flush=True)
    return dict(T=T, book=book, feat=feat, fills=fills)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('-db', default='data/ahc.db')
    ap.add_argument('-out', default='research/cache')
    ap.add_argument('-houses', default='113,116,110')
    o = ap.parse_args()
    os.makedirs(o.out, exist_ok=True)
    db = sqlite3.connect(f'file:{o.db}?mode=ro', uri=True)
    pd.read_sql('select id, name, quality, class, subclass, stackable, sell_price, item_level, required_level from item', db) \
        .set_index('id').to_pickle(f'{o.out}/items.pkl')
    for h in map(int, o.houses.split(',')):
        with open(f'{o.out}/{h}.pkl', 'wb') as fh:
            pickle.dump(build_house(db, h), fh, protocol=pickle.HIGHEST_PROTOCOL)


if __name__ == '__main__':
    main()
