#!/usr/bin/env python3
"""Choose penalty strengths that make the technical score respond to
exposure errors, colour casts and JPEG artefacts (which the human-calibrated
rating misses) without degrading agreement with human ratings.

Tuning data only: BIQ2021 *train* split + the tuning half of the ladder
photos. Report results on held-out data separately.

    tune_penalties.py biq_train.csv biq_train_mos.csv ladder_tune.csv [more_ladder.csv ...]
"""
import csv
import itertools
import sys
from collections import defaultdict
from urllib.parse import unquote

import numpy as np
from scipy.stats import spearmanr
from sklearn.metrics import roc_auc_score


def penalised(r, a, b, g):
    rating = float(r["rating"])
    e = float(r["exposure_score"])
    c = float(r["color_score"])
    blk = float(r.get("blockiness") or 1)
    jp = min(1, max(0, (blk - 1.15) / 0.85))
    return rating * (1 - a * (1 - e)) * (1 - b * (1 - c)) * (1 - g * jp)


q = {unquote(r["image"]): r for r in csv.DictReader(open(sys.argv[1]))}
m = {r["Image Name"]: float(r["MOS"]) for r in csv.DictReader(open(sys.argv[2]))}
keys = [k for k in m if k in q]
y = np.array([m[k] for k in keys])

lad = defaultdict(dict)
for path in sys.argv[3:]:
    for r in csv.DictReader(open(path)):
        lad[r["image"]][(r["degradation"], float(r["level"]))] = r
imgs = [i for i in lad if ("none", 0.0) in lad[i]]
targets = ["over", "under", "warm", "cool", "green", "jpeg"]


def ladder_auc(a, b, g):
    out = {}
    for d in targets:
        levels = sorted({k[1] for i in imgs for k in lad[i] if k[0] == d})
        if d == "jpeg":
            levels = sorted(levels)[:2]  # strongest = lowest quality
        else:
            levels = levels[-2:]
        aucs = []
        for lv in levels:
            pairs = [(penalised(lad[i][("none", 0.0)], a, b, g), penalised(lad[i][(d, lv)], a, b, g)) for i in imgs if (d, lv) in lad[i]]
            if pairs:
                o, dd = zip(*pairs)
                aucs.append(roc_auc_score([1] * len(o) + [0] * len(dd), list(o) + list(dd)))
        out[d] = np.mean(aucs) if aucs else float("nan")
    return out


base = spearmanr([penalised(q[k], 0, 0, 0) for k in keys], y).statistic
print(f"BIQ2021 train SROCC without penalties: {base:.3f}")
best = None
grid = np.round(np.arange(0, 0.85, 0.1), 2)
for a, b, g in itertools.product(grid, grid, grid):
    s = spearmanr([penalised(q[k], a, b, g) for k in keys], y).statistic
    if s < base - 0.01:
        continue
    au = ladder_auc(a, b, g)
    score = np.nanmean(list(au.values()))
    if best is None or score > best[0]:
        best = (score, a, b, g, s, au)
score, a, b, g, s, au = best
print(f"chosen: exposure {a}, colour {b}, compression {g}; BIQ train SROCC {s:.3f}")
print("tuning-ladder AUC (two strongest levels) before -> after:")
b0 = ladder_auc(0, 0, 0)
for d in targets:
    print(f"  {d:<6} {b0[d]:.2f} -> {au[d]:.2f}")
