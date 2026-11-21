"""Demand model: how many units buyers take at or above a list price over the next h hours.

For every (house, scan t, item) and a grid of candidate prices P (multiples of the 24 h median
paid), the label is D = units bought at >= P in (t + 1 h, t + h]. A purchase at >= P only happens
once everything cheaper is gone, so D is how many of your units at P would have sold (first in
line at P) - the same counterfactual the harness fills with. It doesn't depend on what other
sellers cancelled, so there's no censoring to correct for.

The planner's estimate today is the past: units bought at >= P over the last 48 h, halved (per
day). The model has to beat that on later days it never saw.

    .venv/bin/python fillmodel.py            # build the dataset, fit, evaluate, save the model
"""
import pickle, time, sys
import numpy as np, pandas as pd
from harness import Market, H

GRID = np.array([0.80, 0.90, 0.95, 1.00, 1.05, 1.10, 1.20, 1.35])
HORIZON = 24  # hours
FEATURES = ['log_rel_med', 'log_rel_floor', 'log_ahead', 'log_paid24', 'log_paid6', 'paid24_hours',
            'log_listed', 'n_list', 'log_new24', 'trend_floor', 'trend_med', 'log_range24', 'log_price',
            'hour', 'house_bw', 'quality', 'log_past_at', 'log_past48_at']


def label_and_past(fb, ts, prices):
    """For one item: units bought at >= each price in prices, in the future window and the two
    past windows. fb = (ts, price, units, ...) sorted by ts."""
    fts, fpr, fun = fb[0], fb[1], fb[2]
    def at_or_above(a, b):
        i0, i1 = np.searchsorted(fts, a, side='right'), np.searchsorted(fts, b, side='right')
        if i1 <= i0: return np.zeros(len(prices))
        p = fpr[i0:i1]; u = fun[i0:i1]
        o = np.argsort(-p); p = p[o]; cu = np.cumsum(u[o])
        k = np.searchsorted(-p, -prices, side='right')   # count of fills with price >= P
        return np.where(k > 0, cu[np.maximum(k - 1, 0)], 0.0)
    fut = at_or_above(ts + 1 * H, ts + HORIZON * H)
    past24 = at_or_above(ts - 24 * H, ts)
    past48 = at_or_above(ts - 48 * H, ts)
    return fut, past24, past48


def build(m, houses=(113, 116), every=2):
    rows = []
    items = m.items
    for h in houses:
        d = m.h[h]; T = d['T']; fby = d['fills_by']
        usable = [t for t in T[::every] if t >= T[0] + 48 * H and t + HORIZON * H <= T[-1]]
        for t in usable:
            f = d['feat_t'][t]
            f = f[f.med24.notna() & (f.paid24_n >= 3)]
            bk = d['book'][t]
            byitem = {i: g.sort_values('price') for i, g in bk.groupby('item')} if len(bk) else {}
            for item, r in f.iterrows():
                fb = fby.get(item)
                if fb is None: continue
                P = GRID * r.med24
                fut, p24, p48 = label_and_past(fb, t, P)
                g = byitem.get(item)
                if g is not None:
                    cq = np.concatenate([[0], np.cumsum(g.qty.values)])
                    ahead = cq[np.searchsorted(g.price.values, P, side='left')]
                else:
                    ahead = np.zeros(len(P))
                q = items.quality.get(item, 1)
                for j in range(len(GRID)):
                    rows.append((h, t, item, GRID[j], P[j], fut[j], p24[j], p48[j], ahead[j],
                                 r.floor, r.med24, r.paid24_units, r.paid6_units, r.paid24_hours, r.listed, r.n_list,
                                 r.new24, r.trend_floor, r.trend_med, r.min24, r.max24, q))
        print(f'house {h}: {len(rows)} rows so far', flush=True)
    df = pd.DataFrame(rows, columns=['house', 't', 'item', 'mult', 'P', 'D', 'past24_at', 'past48_at', 'ahead',
                                     'floor', 'med24', 'paid24', 'paid6', 'paid24_hours', 'listed', 'n_list',
                                     'new24', 'trend_floor', 'trend_med', 'min24', 'max24', 'quality'])
    return df


def featurize(df):
    X = pd.DataFrame(index=df.index)
    X['log_rel_med'] = np.log(df.mult)
    X['log_rel_floor'] = np.log(df.P / df.floor)
    X['log_ahead'] = np.log1p(df.ahead)
    X['log_paid24'] = np.log1p(df.paid24)
    X['log_paid6'] = np.log1p(df.paid6)
    X['paid24_hours'] = df.paid24_hours
    X['log_listed'] = np.log1p(df.listed)
    X['n_list'] = df.n_list
    X['log_new24'] = np.log1p(df.new24)
    X['trend_floor'] = df.trend_floor.clip(-3, 3)
    X['trend_med'] = df.trend_med.clip(-3, 3)
    X['log_range24'] = np.log(df.max24 / df.min24)
    X['log_price'] = np.log(df.P)
    X['hour'] = pd.to_datetime(df.t, unit='s', utc=True).dt.tz_convert('Europe/Berlin').dt.hour
    X['house_bw'] = (df.house == 116).astype(int)
    X['quality'] = df.quality.fillna(1)
    X['log_past_at'] = np.log1p(df.past24_at)
    X['log_past48_at'] = np.log1p(df.past48_at)
    return X[FEATURES]


def poisson_dev(y, mu):
    mu = np.maximum(mu, 1e-9)
    return 2 * np.mean(np.where(y > 0, y * np.log(np.maximum(y, 1e-9) / mu), 0) - (y - mu))


def evaluate(name, y, mu, df):
    out = dict(model=name, poisson_dev=poisson_dev(y, mu),
               # "will at least one unit sell at P?": how well the ranking separates yes from no
               auc_any=_auc((y > 0).astype(int), mu),
               mean_pred=mu.mean(), mean_true=y.mean())
    return out


def _auc(y, s):
    o = np.argsort(s); y = y[o]; n1 = y.sum(); n0 = len(y) - n1
    if n1 == 0 or n0 == 0: return np.nan
    ranks = np.arange(1, len(y) + 1)
    return (ranks[y == 1].sum() - n1 * (n1 + 1) / 2) / (n1 * n0)


def main():
    from sklearn.ensemble import HistGradientBoostingRegressor
    t0 = time.time()
    m = Market()
    df = build(m)
    print(f'dataset {len(df):,} rows in {time.time() - t0:.0f}s')
    df.to_pickle('cache/fill_dataset.pkl')
    X = featurize(df); y = df.D.values
    # purged split by time: train on scans before split - horizon, test after split
    ts = np.sort(df.t.unique()); split = ts[int(len(ts) * 0.6)]
    tr = (df.t <= split - HORIZON * H).values; te = (df.t >= split).values
    print(f'train {tr.sum():,} rows (to {pd.to_datetime(split - HORIZON*H, unit="s")}), test {te.sum():,} rows (from {pd.to_datetime(split, unit="s")})')
    model = HistGradientBoostingRegressor(loss='poisson', max_iter=400, learning_rate=0.05, max_leaf_nodes=63,
                                          min_samples_leaf=200, l2_regularization=1.0, random_state=0)
    model.fit(X[tr], y[tr])
    mu = model.predict(X[te])
    base24 = df.past24_at.values[te]                      # "the last day repeats"
    base48 = df.past48_at.values[te] / 2                  # the planner's rule: 48 h at >= P, halved
    res = [evaluate('model', y[te], mu, df[te]), evaluate('planner: past 48h at >=P / 2', y[te], base48, df[te]),
           evaluate('past 24h at >=P', y[te], base24, df[te])]
    print(pd.DataFrame(res).round(3).to_string(index=False))
    # calibration by predicted decile
    q = pd.qcut(mu, 10, labels=False, duplicates='drop')
    cal = pd.DataFrame({'pred': mu, 'true': y[te], 'bucket': q}).groupby('bucket').mean()
    print('\ncalibration (test, by predicted decile):'); print(cal.round(1).to_string())
    with open('cache/fill_model.pkl', 'wb') as fh:
        pickle.dump(dict(model=model, features=FEATURES, horizon=HORIZON, grid=GRID), fh)
    # what the model learned: permutation importance on a test sample
    from sklearn.inspection import permutation_importance
    s = np.random.RandomState(0).choice(np.where(te)[0], min(40000, te.sum()), replace=False)
    pi = permutation_importance(model, X.iloc[s], y[s], n_repeats=3, random_state=0,
                                scoring=lambda est, Xs, ys: -poisson_dev(ys, est.predict(Xs)))
    print('\nwhat moves the prediction (permutation importance, deviance increase):')
    print(pd.Series(pi.importances_mean, index=FEATURES).sort_values(ascending=False).round(3).to_string())


if __name__ == '__main__':
    main()
