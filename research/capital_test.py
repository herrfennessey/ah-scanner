"""How each strategy scales with capital, alone and as one portfolio sharing the gold."""
from harness import *
from strategies import make_spread
import reset_test as rt   # make_reset (runs its own demo on import, so we silence it)
pd.set_option('display.width', 250)

def combo(*strats):
    def s(ctx):
        out = []
        for st in strats: out += st(ctx)
        return out
    return s

m = Market()
rows = []
for budget in (100, 200, 500, 1000, 2000, 5000):
    per = 0.3 * budget   # per-item cap scales with the budget
    S = {'H->BW spread': make_spread(max_gold_g=per),
         'BW->H spread': make_spread(buy=116, sell=113, cap=None, max_gold_g=per),
         'cooldown corner': rt.make_reset(0.1, 0.05, demand_frac=0.5)}
    S['all three'] = combo(*S.values())
    for name, st in S.items():
        R = Rules(budget=budget, every_h=6, max_item_frac=0.5 if name == 'cooldown corner' else 0.3, stack=name != 'H->BW spread')
        for sc in ('base', 'half demand'):
            x = summary(run(m, st, replace(R, **STRESS[sc])), name)
            rows.append(dict(budget=budget, strategy=name, scenario=sc, g_day=x.get('g_day_half', np.nan),
                             positions=x.get('positions', 0), turnover=x.get('turnover_g', 0), stuck=x.get('stuck_g', 0)))
df = pd.DataFrame(rows)
for col in ('g_day', 'positions', 'stuck'):
    t = df.pivot_table(index=['strategy', 'scenario'], columns='budget', values=col)
    print(f'\n{col}:'); print(t.round(1).to_string())
df.to_csv('cache/capital_test.csv', index=False)
