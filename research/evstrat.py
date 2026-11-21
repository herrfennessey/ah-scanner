"""Expected-value strategy from the demand model (fillmodel.py), scored against the planner.

At each check, for every item on every route (buy house -> sell house), for each candidate list
price P on the model's grid and each purchase size q along the buy book:

    EV(P, q) = E[min(q, D)] * P * (1 - cut)              what sells
             + (q - E[min(q, D)]) * liq                   unsold stock at half the floor
             - cost(q) - deposit(q) * (1 + P(relist))

D ~ negative binomial with the model's mean (times `share`, the part of that demand you'd get)
and a dispersion fitted on the training days. The best (P, q) per item is kept and trades are
funded by EV per gold. No hand-set gates beyond a minimum EV and return.

    .venv/bin/python evstrat.py   # needs cache/fill_model.pkl and cache/fill_dataset.pkl (fillmodel.py)
"""
import pickle, sys
import numpy as np, pandas as pd
from scipy.stats import nbinom
from harness import *
from strategies import make_spread, make_planner_csv
from fillmodel import GRID, FEATURES, featurize, label_and_past, HORIZON

ROUTES = [(116, 113), (113, 116), (113, 113), (116, 116)]


def load_model(path='cache/fill_model.pkl', dataset='cache/fill_dataset.pkl'):
    with open(path, 'rb') as fh:
        mdl = pickle.load(fh)
    df = pd.read_pickle(dataset)
    ts = np.sort(df.t.unique()); split = ts[int(len(ts) * 0.6)]
    tr = df[df.t <= split - HORIZON * H]
    mu = mdl['model'].predict(featurize(tr)); y = tr.D.values
    # negative binomial size r from Var = mu + mu^2 / r (method of moments), on training rows in
    # the range trades live in (1-100 units predicted): the huge-volume rows would make it look
    # nearly Poisson and overconfident for a few dozen units
    sel = (mu >= 1) & (mu <= 100)
    r = np.mean(mu[sel] ** 2) / max(np.mean((y[sel] - mu[sel]) ** 2 - mu[sel]), 1e-9)
    mdl.update(split=int(split), r=float(r))
    return mdl


def expected_sold(lam, qs, r):
    """E[min(q, D)] for D ~ NB(mean lam, size r), for each q in qs (ascending ints)."""
    if lam <= 0: return np.zeros(len(qs))
    p = r / (r + lam)
    k = np.arange(0, int(qs[-1]))
    sf = nbinom.sf(k, r, p)            # P(D > k)
    c = np.concatenate([[0.0], np.cumsum(sf)])
    return c[np.asarray(qs, int)]


def make_ev(mdl, share=0.5, min_ev_g=0.2, min_roi=0.1, routes=ROUTES, liq_frac=0.5, max_q=2000, max_orders=None):
    model, r = mdl['model'], mdl['r']
    def strat(ctx):
        m = ctx.m; t = ctx.t
        cands = []
        for buy, sell in routes:
            sf = ctx.feat(sell); bbk = ctx.book(buy); sbk = ctx.book(sell)
            if sf is None or bbk is None: continue
            sf = sf[sf.med24.notna() & (sf.paid24_n >= 3)]
            bby = {i: g for i, g in bbk[bbk.last_seen > t].groupby('item')}
            sby = {i: g for i, g in sbk.groupby('item')} if sbk is not None else {}
            items = [i for i in sf.index if i in bby]
            if not items: continue
            rows = []
            fby = m.h[sell]['fills_by']
            for item in items:
                fr = sf.loc[item]; fb = fby.get(item)
                if fb is None: continue
                P = GRID * fr.med24
                _, p24, p48 = label_and_past(fb, t, P)
                g = sby.get(item)
                if g is not None:
                    gp = g.price.values
                    if buy == sell:  # what we'd buy here is ours, not competition
                        pass
                    cq = np.concatenate([[0], np.cumsum(g.qty.values)])
                    ahead = cq[np.searchsorted(gp, P, side='left')]
                else:
                    ahead = np.zeros(len(P))
                for j in range(len(GRID)):
                    rows.append((sell, t, item, GRID[j], P[j], 0, p24[j], p48[j], ahead[j], fr.floor, fr.med24,
                                 fr.paid24_units, fr.paid6_units, fr.paid24_hours, fr.listed, fr.n_list, fr.new24,
                                 fr.trend_floor, fr.trend_med, fr.min24, fr.max24, ctx.items.quality.get(item, 1)))
            if not rows: continue
            df = pd.DataFrame(rows, columns=['house', 't', 'item', 'mult', 'P', 'D', 'past24_at', 'past48_at', 'ahead',
                                             'floor', 'med24', 'paid24', 'paid6', 'paid24_hours', 'listed', 'n_list',
                                             'new24', 'trend_floor', 'trend_med', 'min24', 'max24', 'quality'])
            df['lam'] = model.predict(featurize(df)) * share
            cut = ctx.cut(sell)
            for item, g in df.groupby('item'):
                book = bby[item]
                prices = book.price.values; qty = book.qty.values
                dep_u = ctx.deposit_unit(item, sell)
                fl_now = sf.at[item, 'floor']
                best = None
                for P, lam in zip(g.P.values, g.lam.values):
                    net = P * (1 - cut) - dep_u
                    if prices[0] >= net: continue
                    # purchase sizes at each book level boundary, only levels cheaper than the net
                    ok = prices < net
                    if not ok.any(): continue
                    cq = np.cumsum(qty[ok]); cc = np.cumsum((prices * qty)[ok])
                    cq = np.minimum(cq, max_q); keep = np.concatenate([[True], cq[1:] > cq[:-1]])
                    cq, cc, lv = cq[keep], cc[keep], prices[ok][keep]
                    if not len(cq): continue
                    es = expected_sold(lam, cq.astype(int), r)
                    # unsold units back at half the floor, never more than they cost
                    liq = liq_frac * np.minimum(min(fl_now, P) * (1 - cut), cc / cq)
                    dep = np.maximum(100, dep_u * cq) * (1 + np.clip(1 - es / cq, 0, 1))
                    ev = es * P * (1 - cut) + (cq - es) * liq - cc - dep
                    k = int(np.argmax(ev))
                    if best is None or ev[k] > best[0]:
                        best = (ev[k], P, int(cq[k]), lv[k], cc[k])
                if best and best[0] >= min_ev_g * 1e4 and best[0] >= min_roi * best[4]:
                    ev, P, q, maxp, cost = best
                    cands.append((ev / cost, Order(item, buy, sell, P, maxp, q, tag=f'ev {buy}->{sell}')))
        return [o for _, o in sorted(cands, key=lambda x: -x[0])][:max_orders]
    return strat


if __name__ == '__main__':
    pd.set_option('display.width', 250)
    mdl = load_model()
    m = Market()
    split = mdl['split']
    print('scoring from', pd.to_datetime(split, unit='s'), ' NB size r =', round(mdl['r'], 3))
    R = Rules(budget=100, every_h=4, window_h=24, relist_h=12, t_from=split)
    cols = ['positions', 'turnover_g', 'g_day_zero', 'g_day_half', 'unit_sell', 'losing', 'stuck_g', 'worst_g', 'days']
    S = {'EV model (share 0.5)': make_ev(mdl),
         'EV model (share 0.25)': make_ev(mdl, share=0.25),
         'planner v1.4.1 casual': make_planner_csv(sys.argv[1], 'casual') if len(sys.argv) > 1 else None,
         'planner v1.4.1 active': make_planner_csv(sys.argv[1], 'active') if len(sys.argv) > 1 else None,
         'spread H->BW': make_spread(), 'spread BW->H': make_spread(buy=116, sell=113, cap=None)}
    rows = []
    for name, s in S.items():
        if s is None: continue
        for sc in ('base', 'half demand', 'certain fills only'):
            rows.append(summary(run(m, s, replace(R, **STRESS[sc])), f'{name} | {sc}'))
    print(pd.DataFrame(rows).set_index('label')[cols].round(2).to_string())
    led = run(m, S['EV model (share 0.5)'], R)
    print(); report(led, m, top=8)
