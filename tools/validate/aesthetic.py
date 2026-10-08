#!/usr/bin/env python3
"""Validate the aesthetic score against human ratings.

Ground truth:
  * EVA (Kang, Valenzise & Dufaux 2020; github.com/kang-gnak/eva-dataset,
    CC0): 4,070 photos drawn from AVA, each rated 0-10 for appeal by ~30
    people.
  * AVA means for the same photos (~200 dpchallenge.com voters each; AVA.txt
    from github.com/mtobeiyf/ava_downloader) -- an independent panel.
  * BIQ2021 test (2,000 everyday photos rated for quality by 30 people),
    which the aesthetic model never sees.

    aesthetic.py EVA_DIR AVA.txt EVA_CLIP EVA_QUALITY MODEL_JSON \\
                 BIQ_CLIP BIQ_QUALITY BIQ_MOS [LAION_PTH]

*_CLIP are `validate clip` outputs (the indexer's own decode + CLIP path),
*_QUALITY are `validate quality` outputs, MODEL_JSON is
internal/ml/aesthetic_model.json, BIQ_MOS is BIQ2021's test.csv. LAION_PTH
(sa_0_4_vit_b_32_linear.pth) adds the predictor archivis used before.

EVA numbers for archivis's model come from 10-fold cross-validation with the
shipped recipe (ridge, same alpha search), so every photo is scored by a
model that never saw it.
"""
import csv
import json
import struct
import sys
import zipfile
from collections import defaultdict
from urllib.parse import unquote

import numpy as np
from scipy.stats import pearsonr, spearmanr
from sklearn.linear_model import RidgeCV
from sklearn.metrics import roc_auc_score
from sklearn.model_selection import KFold

(EVA, AVA_TXT, EVA_CLIP, EVA_Q, MODEL, BIQ_CLIP, BIQ_Q, BIQ_MOS) = sys.argv[1:9]
LAION = sys.argv[9] if len(sys.argv) > 9 else None
rng = np.random.default_rng(7)
ALPHAS = np.logspace(-2, 3, 12)
model = json.load(open(MODEL))
Wm, Bm = np.array(model["w"]), model["b"]


def key(name):
    return unquote(name).rsplit(".", 1)[0]


def load_clip(path):
    out = {}
    with open(path) as f:
        for row in csv.reader(f):
            v = np.array(row[1:], dtype=np.float64)
            out[key(row[0])] = v / np.linalg.norm(v)
    return out


def load_tech(path):
    with open(path) as f:
        return {key(r["image"]): float(r["technical"]) for r in csv.DictReader(f)}


def rho(a, b):
    return spearmanr(a, b)[0]


def boot(x, y, n=2000):
    k = len(x)
    vals = [rho(x[s], y[s]) for s in (rng.integers(0, k, k) for _ in range(n))]
    return np.percentile(vals, [2.5, 97.5])


def report(name, x, y):
    lo, hi = boot(x, y)
    print(f"  {name:<52} Spearman {rho(x, y):.3f} [{lo:.3f}, {hi:.3f}]  Pearson {pearsonr(x, y)[0]:.3f}")


def pct(r):  # ml.AestheticPercent
    return np.clip((r - 3) / 5, 0, 1) * 100


# --- EVA -------------------------------------------------------------------
emb = load_clip(EVA_CLIP)
votes = defaultdict(list)
with open(f"{EVA}/data/votes_filtered.csv") as f:
    r = csv.reader(f, delimiter="=")
    next(r)
    for row in r:
        votes[row[0]].append(float(row[2]))
# AVA.txt, one photo per line (AVA readme): row index, image id, counts of
# ratings 1..10, two semantic tag ids, challenge id -- 15 columns. All 4,070
# EVA photos are found by the image id in column 1; column 0 is a row number.
ava = {}
with open(AVA_TXT) as f:
    for line in f:
        p = line.split()
        assert len(p) == 15, f"unexpected AVA.txt line: {line!r}"
        c = np.array(p[2:12], dtype=float)
        if c.sum() > 0:
            ava[p[1]] = float((c * np.arange(1, 11)).sum() / c.sum())
cat = {}
with open(f"{EVA}/data/image_content_category.csv", encoding="utf-8-sig") as f:
    rd = csv.reader(f)
    next(rd)
    for row in rd:
        cat[row[0]] = row[1]

ids = sorted(i for i in votes if i in emb and len(votes[i]) >= 10 and i in ava)
X = np.stack([emb[i] for i in ids])
eva = np.array([np.mean(votes[i]) for i in ids])
avam = np.array([ava[i] for i in ids])
cats = np.array([cat.get(i, "?") for i in ids])
print(f"EVA: {len(ids)} photos, median {int(np.median([len(votes[i]) for i in ids]))} raters each; "
      f"AVA means sd {avam.std():.2f}, range {avam.min():.1f}-{avam.max():.1f}")


def cv_predict(X, y, seed=0):
    out = np.zeros(len(y))
    for tr, te in KFold(10, shuffle=True, random_state=seed).split(X):
        out[te] = RidgeCV(alphas=ALPHAS).fit(X[tr], y[tr]).predict(X[te])
    return out


ours = cv_predict(X, eva)
laion = None
if LAION:
    z = zipfile.ZipFile(LAION)
    lw = np.array(struct.unpack("<512f", z.read("archive/data/0")))
    lb = struct.unpack("<f", z.read("archive/data/1"))[0]
    laion = X @ lw + lb

print("\nAgreement with EVA's raters (photos unseen by archivis's model)")
report("archivis aesthetic", ours, eva)
if laion is not None:
    report("LAION predictor (used before)", laion, eva)

print("\nAgreement with AVA's independent panel, same photos")
report("archivis aesthetic (fit on EVA votes, unseen photos)", ours, avam)
if laion is not None:
    report("LAION predictor", laion, avam)

print("\nHow well people agree with each other")
a, b = [], []
for i in ids:
    v = rng.permutation(votes[i])
    a.append(v[: len(v) // 2].mean())
    b.append(v[len(v) // 2:].mean())
a, b = np.array(a), np.array(b)
sh = rho(a, b)
print(f"  EVA half-panel vs other half (~15 raters each)         Spearman {sh:.3f}")
print(f"  -> reliability of the full EVA panel (Spearman-Brown)   {2*sh/(1+sh):.3f}")
report("EVA panel vs AVA panel", eva, avam)
half = cv_predict(X, a, seed=1)
print(f"  archivis fit on half A's ratings, tested on half B       Spearman {rho(half, b):.3f} "
      f"(half A itself vs half B: {sh:.3f})")

print("\nContent it was not trained on (leave one EVA category out)")
for k in sorted(set(cats)):
    te = cats == k
    p = RidgeCV(alphas=ALPHAS).fit(X[~te], eva[~te]).predict(X[te])
    extra = f"  LAION {rho(laion[te], eva[te]):.3f}" if laion is not None else ""
    print(f"  category {k}  n={te.sum():4d}  archivis {rho(p, eva[te]):.3f}{extra}")

print("\nTraining-set size (held-out set of 470 photos, mean of 5 draws)")
for n in (250, 500, 1000, 2000, 3600):
    rs = []
    for s in range(5):
        p = np.random.default_rng(s).permutation(len(ids))
        tr, te = p[:n], p[3600:]
        rs.append(rho(RidgeCV(alphas=ALPHAS).fit(X[tr], eva[tr]).predict(X[te]), eva[te]))
    print(f"  {n:5d} photos -> {np.mean(rs):.3f}")

q1, q3 = np.percentile(eva, [25, 75])
sel = (eva <= q1) | (eva >= q3)
print(f"\nBest vs worst quarter by EVA rating: archivis ranks the better photo higher "
      f"{roc_auc_score(eva[sel] >= q3, ours[sel])*100:.0f}% of the time"
      + (f" (LAION {roc_auc_score(eva[sel] >= q3, laion[sel])*100:.0f}%)" if laion is not None else ""))

# --- BIQ2021: everyday photos, never seen by the model ---------------------
bemb = load_clip(BIQ_CLIP)
btech = load_tech(BIQ_Q)
with open(BIQ_MOS) as f:
    mos = {key(r["Image Name"]): float(r["MOS"]) for r in csv.DictReader(f)}
bk = sorted(k for k in bemb if k in btech and k in mos)
Xb = np.stack([bemb[k] for k in bk])
bm = np.array([mos[k] for k in bk])
bt = np.array([btech[k] for k in bk])
bours = Xb @ Wm + Bm  # the shipped model
print(f"\nBIQ2021 test: {len(bk)} everyday photos rated for quality (never seen by the aesthetic model)")
report("technical score", bt, bm)
report("archivis aesthetic (shipped model)", bours, bm)
if LAION:
    report("LAION predictor", Xb @ lw + lb, bm)

# --- overall ---------------------------------------------------------------
etech = load_tech(EVA_Q)
et = np.array([etech[i] for i in ids])
print("\nOverall = (1-w)*technical + w*aesthetic%, Spearman vs EVA ratings / BIQ ratings")
for w in (0, .2, .35, .5, .65, .8, 1):
    print(f"  w={w:.2f}  EVA {rho((1-w)*et + w*pct(ours), eva):.3f}  BIQ {rho((1-w)*bt + w*pct(bours), bm):.3f}")
if laion is not None:
    lp = np.clip((laion - 3.5) / 3.5, 0, 1) * 100
    print(f"  before (0.65 technical + 0.35 LAION): EVA {rho(.65*et + .35*lp, eva):.3f}  "
          f"BIQ {rho(.65*bt + .35*np.clip(((Xb @ lw + lb) - 3.5) / 3.5, 0, 1)*100, bm):.3f}")

print("\nScale: predicted rating percentiles 1/5/50/95/99")
print("  EVA (contest photos):   " + " ".join(f"{v:.2f}" for v in np.percentile(ours, [1, 5, 50, 95, 99])))
print("  BIQ (everyday photos):  " + " ".join(f"{v:.2f}" for v in np.percentile(bours, [1, 5, 50, 95, 99])))
print(f"  clipped by 3->0, 8->100: EVA {np.mean((ours < 3) | (ours > 8))*100:.1f}%, "
      f"BIQ {np.mean((bours < 3) | (bours > 8))*100:.1f}%")
