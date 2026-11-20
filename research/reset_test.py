"""Idea 3: cornering capped-supply (daily-cooldown) markets on Horde, against buy-and-hold."""
import sys
from harness import *
pd.set_option('display.width', 250); pd.set_option('display.max_columns', 30)
COOLDOWN = {12360: 'Arcanite Bar', 15409: 'Refined Deeprock Salt', 14342: 'Mooncloth', 15407: 'Cured Rugged Hide'}

def make_reset(premium=0.1, margin=0.1, demand_frac=1.0, house=113, items=COOLDOWN):
    """Buy the cheap end up to a cap and list at T = median paid x (1+premium): the corner."""
    def strat(ctx):
        f = ctx.feat(house); out = []
        if f is None: return out
        cut = ctx.cut(house)
        for iid in items:
            if iid not in f.index or pd.isna(f.at[iid, 'med24']): continue
            r = f.loc[iid]
            T = r.med24 * (1 + premium)
            dep = ctx.deposit_unit(iid, house)
            maxp = (T * (1 - cut) - dep) / (1 + margin)
            if r.floor > maxp: continue
            out.append(Order(iid, house, house, T, maxp, int(demand_frac * r.paid24_units), tag='reset'))
        return out
    return strat

def make_hold(units_frac=0.25, house=113, items=COOLDOWN):
    """Benchmark: buy at the floor every check regardless, list at the floor (no edge, pure trend)."""
    def strat(ctx):
        f = ctx.feat(house); out = []
        for iid in items:
            if f is None or iid not in f.index: continue
            r = f.loc[iid]
            out.append(Order(iid, house, house, r.floor, r.floor, int(units_frac * r.paid24_units), tag='hold'))
        return out
    return strat

if __name__ == "__main__":
    m = Market()
    cols = ['positions', 'turnover_g', 'g_day_zero', 'g_day_half', 'unit_sell', 'losing', 'stuck_g', 'worst_g']
    rows = []
    for budget in (100, 500, 2000):
        R = Rules(budget=budget, every_h=6, max_item_frac=0.5, window_h=48)
        rows.append(summary(run(m, make_hold(), R), f'{budget}g | benchmark: buy floor, list at floor'))
        for prem in (0.0, 0.1, 0.2):
            for mg in (0.05, 0.15):
                rows.append(summary(run(m, make_reset(prem, mg), R), f'{budget}g | corner +{prem:.0%} margin {mg:.0%}'))
    print(pd.DataFrame(rows).set_index('label')[cols].round(2).to_string())
    print('\nstress, 2000g, corner +10% margin 5%:')
    print(stress(m, make_reset(0.1, 0.05), Rules(budget=2000, every_h=6, max_item_frac=0.5)).round(2)[cols])
    print('\ntrain/test (purged, 24 h holds), 2000g:')
    print(sweep(m, make_reset, dict(premium=[0.0, 0.1, 0.2], margin=[0.05, 0.15]), Rules(budget=2000, every_h=6, max_item_frac=0.5, window_h=24, relist_h=12)).round(2).to_string(index=False))
    led = run(m, make_reset(0.1, 0.05), Rules(budget=2000, every_h=6, max_item_frac=0.5))
    print(); report(led, m, top=4)
