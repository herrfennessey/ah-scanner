"""Worked example: score strategies, stress them, sweep parameters on train and read test.

    .venv/bin/python example.py
"""
import time
from harness import *
from strategies import make_spread, make_dip, make_planner_csv

pd.set_option('display.width', 250); pd.set_option('display.max_columns', 20)
t0 = time.time()
m = Market()
print(f'loaded in {time.time() - t0:.1f}s')

# 1. one strategy, full report
led = run(m, make_spread(buy=113, sell=116, cap=110), Rules(budget=100, every_h=6))
report(led, m)

# 2. adversarial scenarios
print(stress(m, make_spread()).round(2)[['positions', 'g_day_zero', 'g_day_half', 'unit_sell', 'losing', 'stuck_g']])

# 3. parameter sweep: choose on the first half (train), read the second half (test)
t0 = time.time()
grid = dict(margin=[0.1, 0.15, 0.25], demand_frac=[0.1, 0.25, 0.5], min_hours=[3, 6])
sw = sweep(m, make_spread, grid, Rules(every_h=6))
print(f'\nsweep of {len(sw)} runs in {time.time() - t0:.1f}s (g/day, unsold at half the floor)')
print(sw.round(2).to_string(index=False))
