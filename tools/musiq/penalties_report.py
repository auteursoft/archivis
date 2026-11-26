#!/usr/bin/env python3
"""Held-out report for technical-score penalty strengths (FIT.md, "Penalties").

    penalties_report.py biq_test.csv biq_test_mos.csv ladder_hold.csv EXP,COL,JPG

biq_test.csv and ladder_hold.csv are `validate quality -models` and
`validate ladder -models` output; EXP,COL,JPG are the new strengths (from
tune_penalties.py --rating-col rating_musiq on tuning data). Prints:

- Spearman vs BIQ2021 test MOS with 95% bootstrap CIs (2,000 resamples of
  the photos, numpy default_rng(0), 2.5th/97.5th percentiles), and paired CIs
  of differences on the same resamples;
- worst-vs-best quarter AUC on BIQ2021 test;
- per-defect, per-severity AUC on the held-out ladder photos: originals vs
  degraded versions (sklearn roc_auc_score over all originals and all versions
  at that level), the definition VALIDATION.md's table and tune_penalties.py
  use.

Today's strengths are 0.1, 0.1, 0.2 (quality.Technical).
"""
import csv
import sys
from collections import defaultdict
from urllib.parse import unquote

import numpy as np
from scipy.stats import rankdata, spearmanr
from sklearn.metrics import roc_auc_score

TODAY = (0.1, 0.1, 0.2)


def pen(r, col, s):
    a, b, g = s
    e, c = float(r["exposure_score"]), float(r["color_score"])
    jp = min(1, max(0, (float(r["blockiness"]) - 1.15) / 0.85))
    return float(r[col]) * (1 - a * (1 - e)) * (1 - b * (1 - c)) * (1 - g * jp)


def scores(new):
    """name -> function(row) for every score reported."""
    return {
        "rating (today, no MUSIQ)": lambda r: float(r["rating"]),
        "technical shipped (rating × today's)": lambda r: pen(r, "rating", TODAY),
        "MUSIQ alone": lambda r: float(r["musiq"]),
        "rating_musiq": lambda r: float(r["rating_musiq"]),
        "rating_musiq × today's": lambda r: pen(r, "rating_musiq", TODAY),
        "rating_musiq × new": lambda r: pen(r, "rating_musiq", new),
    }


def spearman_fast(ranks_y, x):
    return np.corrcoef(rankdata(x), ranks_y)[0, 1]


def main():
    qpath, mospath, ladpath, strengths = sys.argv[1:5]
    new = tuple(float(v) for v in strengths.split(","))
    S = scores(new)
    q = {unquote(r["image"]): r for r in csv.DictReader(open(qpath))}
    mos = {r["Image Name"]: float(r["MOS"]) for r in csv.DictReader(open(mospath))}
    keys = [k for k in mos if k in q]
    y = np.array([mos[k] for k in keys])
    X = {n: np.array([f(q[k]) for k in keys]) for n, f in S.items()}
    print(f"new strengths: exposure {new[0]}, colour {new[1]}, compression {new[2]}")
    print(f"\nBIQ2021 test ({len(keys)} photos): Spearman [95% bootstrap CI], Pearson, worst-vs-best quarter AUC")
    rng = np.random.default_rng(0)
    idx = [rng.integers(0, len(y), len(y)) for _ in range(2000)]
    boot = {n: np.array([spearmanr(x[i], y[i]).statistic for i in idx]) for n, x in X.items()}
    lo, hi = np.quantile(y, [0.25, 0.75])
    sel = (y <= lo) | (y >= hi)
    for n, x in X.items():
        s = spearmanr(x, y).statistic
        c = np.percentile(boot[n], [2.5, 97.5])
        auc = roc_auc_score(y[sel] >= hi, x[sel])
        print(f"  {n:<40} {s:.3f} [{c[0]:.3f}, {c[1]:.3f}]  {np.corrcoef(x, y)[0, 1]:.3f}  {auc:.3f}")
    print("\nPaired differences (same resamples): Δ Spearman [95% CI]")
    for a, b in [
        ("rating_musiq × new", "rating_musiq × today's"),
        ("rating_musiq × new", "technical shipped (rating × today's)"),
        ("rating_musiq × today's", "technical shipped (rating × today's)"),
        ("rating_musiq × new", "rating_musiq"),
        ("rating_musiq × today's", "rating_musiq"),
        ("rating_musiq × new", "MUSIQ alone"),
        ("technical shipped (rating × today's)", "rating (today, no MUSIQ)"),
    ]:
        d = spearmanr(X[a], y).statistic - spearmanr(X[b], y).statistic
        c = np.percentile(boot[a] - boot[b], [2.5, 97.5])
        print(f"  {a} − {b}: {d:+.3f} [{c[0]:+.3f}, {c[1]:+.3f}]")

    lad = defaultdict(dict)
    for r in csv.DictReader(open(ladpath)):
        lad[r["image"]][(r["degradation"], float(r["level"]))] = r
    imgs = sorted(i for i in lad if ("none", 0.0) in lad[i])
    degs = []
    for i in imgs:
        for d, _ in lad[i]:
            if d != "none" and d not in degs:
                degs.append(d)

    def auc(f, d, lv):
        o = [f(lad[i][("none", 0.0)]) for i in imgs if (d, lv) in lad[i]]
        g = [f(lad[i][(d, lv)]) for i in imgs if (d, lv) in lad[i]]
        return roc_auc_score([1] * len(o) + [0] * len(g), o + g)

    def levels(d):
        lv = sorted({k[1] for i in imgs for k in lad[i] if k[0] == d})
        return lv[::-1] if d == "jpeg" else lv  # mild -> severe (JPEG: high quality first)

    comp = {"focus": "focus_score", "exposure": "exposure_score", "colour": "color_score", "noise": "noise_score"}
    own = {"blur": "focus", "motion": "focus", "noise": "noise", "over": "exposure", "under": "exposure",
           "warm": "colour", "cool": "colour", "green": "colour", "blur+noise": "focus"}
    print(f"\nHeld-out ladder ({len(imgs)} photos): AUC by severity, mild -> severe")
    print("\nOwn sub-score (rebuild check against VALIDATION.md):")
    for d in degs:
        if d in own:
            f = lambda r, c=comp[own[d]]: float(r[c])
            print(f"  {d:<11} {own[d]:<9} " + " / ".join(f"{auc(f, d, lv):.2f}" for lv in levels(d)))
    cols = ["rating (today, no MUSIQ)", "technical shipped (rating × today's)", "rating_musiq",
            "rating_musiq × today's", "rating_musiq × new"]
    print("\nTechnical scores:")
    print("  " + " | ".join(["defect", "levels"] + cols))
    for d in degs:
        lv = levels(d)
        cells = [" / ".join(f"{auc(S[c], d, l):.2f}" for l in lv) for c in cols]
        print("  " + " | ".join([d, " / ".join(f"{l:g}" for l in lv)] + cells))


if __name__ == "__main__":
    main()
