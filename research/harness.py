"""Strategy test harness for commodity trading on the recorded auction history.

A strategy is a function  strategy(ctx) -> list[Order]  called at every check time. It sees only
the past: ctx.feat(house) (features at the latest scan <= t, indexed by item), ctx.book(house)
(the bottom of each item's order book then), ctx.history(house, hours) (feature rows over the
last hours), ctx.cash, ctx.holding(...), ctx.items. Build the cache first: python research/build.py

The executor then plays each order against what really happened:
  * buys the listed units cheapest first, up to max_price / max_units / max_gold, only listings
    still there at the next scan (you get to the AH within the hour) and never one bought before;
  * posts them on the sell house after post_delay_h at list_price, paying the deposit;
  * fills them only from later real purchases on the sell house at >= that price (a buyer who
    paid more would have taken the cheaper units first), each purchase used once across all
    positions, scaled by `share` (the fraction of that demand you'd capture);
  * at relist_h relists what's left at min(price, floor - 1c) (another deposit), and at window_h
    values what's still unsold three ways: zero / half the floor / the floor (after the cut).
Cash comes back when purchases fill, so capital is recycled only as fast as things really sell.

    from harness import Market, Rules, run, report, stress, sweep
    m = Market()                       # loads research/cache
    led = run(m, my_strategy, Rules(budget=100, every_h=6))
    report(led, m)
"""
import heapq, os, pickle
from dataclasses import dataclass, field, replace
from typing import Callable, Optional
import numpy as np, pandas as pd

H = 3600
HOUSE = {'horde': 113, 'blackwater': 116, 'alliance': 110}
TRADABLE = {113, 116}
CACHE = os.path.join(os.path.dirname(os.path.abspath(__file__)), 'cache')


@dataclass
class Order:
    item: int
    buy: int                 # house to buy on
    sell: int                # house to sell on
    list_price: float        # copper per unit
    max_price: float         # buy listings priced at most this
    max_units: int = 10**9
    max_gold: float = float('inf')   # copper
    tag: str = ''


@dataclass
class Rules:
    budget: float = 100.0            # gold
    every_h: float = 6               # hours between checks
    warm_h: float = 24               # history before the first check
    window_h: float = 48             # hold horizon; unsold stock valued then
    relist_h: float = 24             # relist what's left after this
    post_delay_h: float = 1          # buy -> posted on the sell house
    require_next: bool = True        # only listings still up at the next scan
    share: float = 1.0               # fraction of later demand at >= price you capture
    certain_only: bool = False       # count only partial buys as demand (drops whole-lot 'sold')
    max_item_frac: float = 0.3       # per item, of the budget
    stack: bool = False              # allow a second position in an item already held
    dep_rate: float = 0.20           # deposit = vendor price x this per listing (min 1s)
    dep_mult: dict = field(default_factory=lambda: {116: 5.0})
    cut: dict = field(default_factory=lambda: {113: 0.05, 116: 0.15})
    liq: str = 'half'                # how unsold stock comes back as cash: zero | half | floor
    relist_floor_frac: float = 0.0   # never relist under cost x this (0 = may relist at a loss)
    t_from: Optional[int] = None     # restrict check times
    t_to: Optional[int] = None


class Market:
    def __init__(self, cache=CACHE, houses=(113, 116, 110)):
        self.items = pd.read_pickle(f'{cache}/items.pkl')
        self.h = {}
        for h in houses:
            with open(f'{cache}/{h}.pkl', 'rb') as f:
                d = pickle.load(f)
            fl = d['fills']
            by = {}
            for item, g in fl.groupby('item', sort=False):
                by[item] = (g.ts.values, g.price.values.astype(float), g.units.values.astype(float), g.aid.values, g.certain.values)
            d['fills_by'] = by
            d['feat_t'] = {t: g.droplevel(0) for t, g in d['feat'].groupby(level=0)}
            self.h[h] = d
        self.T = np.array(sorted(self.h[113]['T']))
        self.end = min(d['T'][-1] for d in self.h.values())
        self.start = max(d['T'][0] for d in self.h.values())

    def asof(self, h, t):
        T = self.h[h]['T']; i = np.searchsorted(T, t, side='right') - 1
        return T[i] if i >= 0 else None

    def feat(self, h, t):
        s = self.asof(h, t)
        return self.h[h]['feat_t'].get(s) if s is not None else None

    def book(self, h, t):
        s = self.asof(h, t)
        return self.h[h]['book'].get(s) if s is not None else None

    def floor(self, h, item, t):
        f = self.feat(h, t)
        if f is None or item not in f.index: return np.nan
        return f.at[item, 'floor']

    def history(self, h, t, hours):
        f = self.h[h]['feat']
        ts = f.index.get_level_values(0)
        return f[(ts > t - hours * H) & (ts <= t)]

    def name(self, item):
        return self.items.name.get(item, str(item))


class Ctx:
    def __init__(self, m, t, cash, rules, held):
        self.m, self.t, self.cash, self.rules, self._held = m, t, cash, rules, held
        self.items = m.items
        self.budget = rules.budget * 1e4

    def feat(self, h): return self.m.feat(h, self.t)
    def book(self, h): return self.m.book(h, self.t)
    def history(self, h, hours): return self.m.history(h, self.t, hours)
    def holding(self, sell, item): return self._held.get((sell, item), 0)
    def deposit_unit(self, item, sell):
        sp = self.items.sell_price.get(item, 0) or 0
        return self.rules.dep_rate * sp * self.rules.dep_mult.get(sell, 1.0)
    def cut(self, sell): return self.rules.cut.get(sell, 0.05)


def _deposit(m, rules, item, sell, units):
    sp = m.items.sell_price.get(item, 0) or 0
    return max(100.0, rules.dep_rate * sp * rules.dep_mult.get(sell, 1.0) * units)


def run(m: Market, strategy: Callable, rules: Rules = Rules(), name: str = '') -> pd.DataFrame:
    """Plays the strategy and returns one ledger row per position."""
    r = rules
    last_open = m.end - r.window_h * H          # every position must resolve inside the data
    checks = [t for t in m.T if t >= m.start + r.warm_h * H and t <= last_open]
    sel, prev = [], None
    for t in checks:
        if (r.t_from and t < r.t_from) or (r.t_to and t >= r.t_to): continue
        if prev is None or t - prev >= r.every_h * H - 300:
            sel.append(t); prev = t
    cash = r.budget * 1e4
    flows = []                 # (ts, amount, key)
    held = {}
    bought = {113: set(), 116: set(), 110: set()}
    used = {}                  # (house, item) -> used units per fill index
    rows = []
    for t in sel:
        while flows and flows[0][0] <= t:
            ts, amt, key, closes = heapq.heappop(flows)
            cash += amt
            if closes:
                held[key] = held.get(key, 0) - closes
                if held[key] <= 0: held.pop(key, None)
        ctx = Ctx(m, t, cash, r, held)
        orders = strategy(ctx) or []
        spent_item = {}
        for o in orders:
            if o.buy not in TRADABLE or o.sell not in TRADABLE: continue
            key = (o.sell, o.item)
            if not r.stack and key in held: continue
            bk = m.book(o.buy, t)
            if bk is None: continue
            lots = bk[(bk.item == o.item) & (bk.price <= o.max_price)]
            if r.require_next: lots = lots[lots.last_seen > t]
            cap = min(cash, o.max_gold, r.max_item_frac * r.budget * 1e4 - spent_item.get(o.item, 0))
            units = 0; cost = 0.0; aids = []
            for price, qty, aid in zip(lots.price.values, lots.qty.values, lots.aid.values):
                if aid in bought[o.buy]: continue
                take = int(min(qty, o.max_units - units, (cap - cost) // price))
                if take <= 0: break
                units += take; cost += take * price; aids.append(aid)
            if units == 0: continue
            for aid in aids: bought[o.buy].add(aid)
            cash -= cost; spent_item[o.item] = spent_item.get(o.item, 0) + cost
            row, cf = _play(m, r, o, t, units, cost, bought, used)
            cash -= _deposit(m, r, o.item, o.sell, units)
            row.update(t=t, item=o.item, name=m.name(o.item), buy=o.buy, sell=o.sell, tag=o.tag, strategy=name)
            rows.append(row)
            held[key] = held.get(key, 0) + 1
            for i, (ts, amt) in enumerate(cf):
                heapq.heappush(flows, (ts, amt, key, 1 if i == len(cf) - 1 else 0))
    led = pd.DataFrame(rows)
    led.attrs['checks'] = sel; led.attrs['rules'] = r
    return led


def _play(m, r, o, t, units, cost, bought, used):
    cut = r.cut.get(o.sell, 0.05)
    fb = m.h[o.sell]['fills_by'].get(o.item)
    key = (o.sell, o.item)
    if fb is not None and key not in used: used[key] = np.zeros(len(fb[0]))
    rem = units; deps = _deposit(m, r, o.item, o.sell, units); rev = 0.0
    cf = []; last = t; relists = 0
    P = o.list_price
    phases = [(t + r.post_delay_h * H, t + r.relist_h * H), (t + r.relist_h * H, t + r.window_h * H)]
    for k, (a, b) in enumerate(phases):
        if rem <= 0: break
        if k == 1:
            fl = m.floor(o.sell, o.item, a)
            if not np.isnan(fl): P = min(P, fl - 1)
            P = max(P, cost / units * r.relist_floor_frac, 1)
            d = _deposit(m, r, o.item, o.sell, rem); deps += d; cf.append((a, -d)); relists += 1
        if fb is None: continue
        ts, pr, un, aid, cert = fb
        i0, i1 = np.searchsorted(ts, a, side='left'), np.searchsorted(ts, b, side='right')
        u = used[key]
        for j in range(i0, i1):
            if pr[j] < P or (r.certain_only and not cert[j]) or aid[j] in bought[o.sell]: continue
            avail = un[j] * r.share - u[j]
            if avail <= 0: continue
            take = min(avail, rem); u[j] += take; rem -= take
            amt = take * P * (1 - cut); rev += amt; cf.append((ts[j], amt)); last = ts[j]
            if rem <= 1e-9: rem = 0; break
    end = t + r.window_h * H
    fl_end = m.floor(o.sell, o.item, end)
    if np.isnan(fl_end): fl_end = 0.0
    liq_unit = min(fl_end, P) * (1 - cut)
    liq = {'zero': 0.0, 'half': 0.5 * liq_unit * rem, 'floor': liq_unit * rem}
    cf.append((end if rem > 0 else last, liq[r.liq]))
    base = rev - cost - deps
    row = dict(units=units, cost=cost, list_price=o.list_price, sold=units - rem, revenue=rev, deposits=deps,
               relists=relists, hours=(last - t) / H, pnl_zero=base, pnl_half=base + liq['half'], pnl_floor=base + liq['floor'],
               stuck=cost * rem / units)
    return row, sorted(cf)


def days_of(led):
    ch = led.attrs['checks']; r = led.attrs['rules']
    return (ch[-1] - ch[0]) / 86400 + r.every_h / 24 if ch else 1


def summary(led, label=''):
    if led is None or not len(led):
        return dict(label=label, positions=0)
    d = days_of(led)
    return dict(label=label, positions=len(led), turnover_g=led.cost.sum() / 1e4,
                g_day_zero=led.pnl_zero.sum() / 1e4 / d, g_day_half=led.pnl_half.sum() / 1e4 / d,
                g_day_floor=led.pnl_floor.sum() / 1e4 / d,
                unit_sell=led.sold.sum() / led.units.sum(), sold_out=(led.sold >= led.units - 1e-9).mean(),
                losing=(led.pnl_half < 0).mean(), stuck_g=led.stuck.sum() / 1e4, worst_g=led.pnl_half.min() / 1e4,
                med_hours=led.hours.median(), days=d)


def report(led, m=None, top=10):
    s = summary(led)
    print(pd.Series(s).to_string())
    if not len(led): return
    led = led.assign(day=pd.to_datetime(led.t, unit='s').dt.date, route=led.buy.astype(str) + '->' + led.sell.astype(str))
    print('\nby day (pnl_half, gold):'); print((led.groupby('day').pnl_half.sum() / 1e4).round(1).to_string())
    print('\nby route:'); print(led.groupby('route').agg(n=('cost', 'size'), cost=('cost', lambda x: x.sum() / 1e4), pnl_half=('pnl_half', lambda x: x.sum() / 1e4), sell=('sold', 'sum')).round(1).to_string())
    g = led.groupby('name').agg(n=('cost', 'size'), cost=('cost', lambda x: x.sum() / 1e4), pnl=('pnl_half', lambda x: x.sum() / 1e4)).sort_values('pnl')
    print(f'\nbest items:\n{g.tail(top)[::-1].round(2).to_string()}\nworst items:\n{g.head(top).round(2).to_string()}')


STRESS = {
    'base': {},
    'half demand': {'share': 0.5},
    'certain fills only': {'certain_only': True},
    'slow (3h to post)': {'post_delay_h': 3},
    'deposits x2': {'dep_rate': 0.40},
    'unsold = 0': {'liq': 'zero'},
}


def stress(m, strategy, rules=Rules(), scenarios=STRESS):
    """Runs the strategy under each adversarial scenario; returns a table."""
    out = []
    for k, ov in scenarios.items():
        out.append(summary(run(m, strategy, replace(rules, **ov)), k))
    return pd.DataFrame(out).set_index('label')


def split_ts(m, frac=0.5):
    """A check time splitting the usable checks into train (before) and test (after)."""
    return int(m.start + (m.end - m.start) * frac)


def sweep(m, make_strategy, grid: dict, rules=Rules(), split=None, metric='g_day_half'):
    """make_strategy(**params) -> strategy. Each combination is run once; the ledger is split by
    open time so you choose on train and read the result on test. Sorted by train."""
    import itertools
    split = split or split_ts(m)
    keys = list(grid)
    out = []
    for vals in itertools.product(*grid.values()):
        p = dict(zip(keys, vals))
        led = run(m, make_strategy(**p), rules)
        row = dict(p)
        if len(led):
            tr, te = led[led.t < split], led[led.t >= split]
            ch = led.attrs['checks']
            dtr = max((split - ch[0]) / 86400, 1e-9); dte = max((ch[-1] - split) / 86400 + rules.every_h / 24, 1e-9)
            row.update(train=tr['pnl_' + metric.split('_')[-1]].sum() / 1e4 / dtr, test=te['pnl_' + metric.split('_')[-1]].sum() / 1e4 / dte,
                       n_train=len(tr), n_test=len(te), all=summary(led)[metric])
        out.append(row)
    return pd.DataFrame(out).sort_values('train', ascending=False)
