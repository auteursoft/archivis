#!/usr/bin/env python3
"""Benchmark MUSIQ against archivis's technical and aesthetic scores.

    bench.py -out DIR \\
      -koniq-meta koniq_meta.csv -koniq-scores koniq_test.csv \\
      -biq-train-q biq_train_quality.csv -biq-test-q biq_test_quality.csv \\
      -biq-train-mos "Train (Images and MOS).csv" -biq-test-mos "Test (Images and MOS).csv" \\
      -biq-musiq-train biq_train_musiq.csv -biq-musiq-test biq_test_musiq.csv \\
      -eva EVA_DIR -ava AVA.txt -ava-test generic_test.jpgl -eva-clip eva_clip.csv -eva-musiq eva_musiq.csv \\
      [-speed speed.csv]

Inputs:
  * *_quality.csv: `go run ./tools/validate quality` on BIQ2021's official
    train/test images (archivis's measurements and its shipped rating);
  * eva_clip.csv: `go run ./tools/validate clip` on EVA_DIR/images/EVA_together;
  * *_musiq.csv / koniq_test.csv: tools/musiq/score.py output;
  * koniq_meta.csv: KonIQ-10k MOS with the official train/valid/test sets;
  * generic_test.jpgl: AVA's standard test list (for the leakage check).

Data handling follows tools/validate/fit_quality.py (BIQ2021 rows matched by
name, features and log transforms as RAW below, quadratic ridge as shipped)
and tools/validate/aesthetic.py (EVA photos with >= 10 votes that are in
AVA.txt, L2-normalised CLIP embedding, ridge with the same alphas, 10-fold
KFold(shuffle, random_state=0)). Confidence intervals are 95% percentile
bootstraps over photos (2,000 resamples), as in aesthetic.py.

Writes DIR/results.csv (one row per dataset x method) and prints a summary.
"""
import argparse
import csv
import os
from collections import defaultdict
from urllib.parse import unquote

import numpy as np
from scipy.stats import pearsonr, spearmanr
from sklearn.linear_model import RidgeCV
from sklearn.model_selection import KFold
from sklearn.pipeline import make_pipeline
from sklearn.preprocessing import PolynomialFeatures, StandardScaler

# fit_quality.py's RAW inputs and transforms (quality.RateMetrics).
RAW = ["sharpness", "sharpness_global", "brightness", "highlights", "shadows", "dynamic_range", "contrast",
       "lab_cast", "colorfulness", "noise"]
LOG = [0, 1, 9]
TECH = ["koniq", "spaq", "paq2piq"]
ALPHAS_Q = np.logspace(-2, 4, 13)  # fit_quality.export_quadratic
ALPHAS_A = np.logspace(-2, 3, 12)  # aesthetic.py / fit_aesthetic.py

ap = argparse.ArgumentParser()
for k in ["out", "koniq-meta", "koniq-scores", "biq-train-q", "biq-test-q", "biq-train-mos", "biq-test-mos",
          "biq-musiq-train", "biq-musiq-test", "eva", "ava", "ava-test", "eva-clip", "eva-musiq", "speed"]:
    ap.add_argument("-" + k)
a = ap.parse_args()
os.makedirs(a.out, exist_ok=True)
rng = np.random.default_rng(7)
results = []


def rho(x, y):
    return spearmanr(x, y)[0]


def boot(x, y, n=2000):
    k = len(x)
    vals = [rho(x[s], y[s]) for s in (rng.integers(0, k, k) for _ in range(n))]
    return np.percentile(vals, [2.5, 97.5])


def report(dataset, method, x, y, note=""):
    x, y = np.asarray(x, float), np.asarray(y, float)
    lo, hi = boot(x, y)
    r = dict(dataset=dataset, method=method, n=len(x), spearman=round(rho(x, y), 4), ci_lo=round(lo, 4),
             ci_hi=round(hi, 4), pearson=round(pearsonr(x, y)[0], 4), note=note)
    results.append(r)
    print(f"  {method:<58} n={len(x):5d}  Spearman {r['spearman']:.3f} [{lo:.3f}, {hi:.3f}]  Pearson {r['pearson']:.3f}"
          + (f"  ({note})" if note else ""))


def read_musiq(path):
    with open(path) as f:
        return {unquote(r["image"]): {k: float(v) for k, v in r.items() if k != "image"} for r in csv.DictReader(f)}


# --- KonIQ-10k: correctness gate ------------------------------------------
if a.koniq_meta and a.koniq_scores:
    with open(a.koniq_meta) as f:
        meta = {r["img_name"]: r for r in csv.DictReader(f)}
    ks = read_musiq(a.koniq_scores)
    keys = sorted(k for k in ks if meta[k]["official_split"] == "test")
    print(f"\nKonIQ-10k official test split: {len(keys)} photos (1024x768), MUSIQ paper: Spearman 0.916")
    report("KonIQ-10k test", "MUSIQ koniq checkpoint", [ks[k]["koniq"] for k in keys],
           [float(meta[k]["mos"]) for k in keys], "correctness gate; paper 0.916 / PLCC 0.928")

# --- BIQ2021: technical quality ---------------------------------------------
if a.biq_train_q:
    def load(qpath, mpath, mus):
        q = {unquote(r["image"]): r for r in csv.DictReader(open(qpath))}
        m = {r["Image Name"]: float(r["MOS"]) for r in csv.DictReader(open(mpath))}
        keys = sorted(k for k in m if k in q and k in mus)
        missing = sorted(k for k in m if k not in q or k not in mus)
        return keys, q, np.array([m[k] for k in keys]), missing

    def feats(keys, q):
        X = np.array([[float(q[k][c]) for c in RAW] for k in keys])
        X[:, LOG] = np.log1p(X[:, LOG])
        return X

    mtr, mte = read_musiq(a.biq_musiq_train), read_musiq(a.biq_musiq_test)
    ktr, qtr, ytr, miss_tr = load(a.biq_train_q, a.biq_train_mos, mtr)
    kte, qte, yte, miss_te = load(a.biq_test_q, a.biq_test_mos, mte)
    print(f"\nBIQ2021 official split: train {len(ktr)} / test {len(kte)} photos "
          f"(missing from a harness: train {len(miss_tr)}, test {len(miss_te)} {miss_te[:3]})")
    Xtr, Xte = feats(ktr, qtr), feats(kte, qte)
    Mtr = np.array([[mtr[k][c] for c in TECH] for k in ktr])
    Mte = np.array([[mte[k][c] for c in TECH] for k in kte])

    def quad():  # the shipped rating model's recipe (fit_quality.export_quadratic)
        return make_pipeline(StandardScaler(), PolynomialFeatures(2), StandardScaler(), RidgeCV(alphas=ALPHAS_Q))

    def lin():
        return make_pipeline(StandardScaler(), RidgeCV(alphas=ALPHAS_Q))

    D = "BIQ2021 test"
    report(D, "archivis rating as shipped (Go, 'rating' column)", [float(qte[k]["rating"]) for k in kte], yte,
           "VALIDATION.md: 0.632")
    report(D, "archivis rating refit here (quadratic ridge on train)", quad().fit(Xtr, ytr).predict(Xte), yte,
           "same recipe, reproduces the above")
    report(D, "archivis technical score as shipped", [float(qte[k]["technical"]) for k in kte], yte,
           "VALIDATION.md: 0.628")
    for i, c in enumerate(TECH):
        report(D, f"MUSIQ {c} checkpoint (no refit)", Mte[:, i], yte)
    for i, c in enumerate(TECH):
        report(D, f"blend: archivis measurements + MUSIQ {c} (quadratic, refit)",
               quad().fit(np.c_[Xtr, Mtr[:, i]], ytr).predict(np.c_[Xte, Mte[:, i]]), yte)
    for i, c in enumerate(TECH):
        report(D, f"blend: archivis measurements + MUSIQ {c} (linear, refit)",
               lin().fit(np.c_[Xtr, Mtr[:, i]], ytr).predict(np.c_[Xte, Mte[:, i]]), yte)
    report(D, "blend: MUSIQ koniq+spaq+paq2piq (linear, refit)", lin().fit(Mtr, ytr).predict(Mte), yte)
    report(D, "blend: archivis measurements + all three MUSIQ (quadratic, refit)",
           quad().fit(np.c_[Xtr, Mtr], ytr).predict(np.c_[Xte, Mte]), yte)

# --- EVA: aesthetics ----------------------------------------------------------
if a.eva:
    def key(name):
        return unquote(name).rsplit(".", 1)[0]

    emb = {}
    with open(a.eva_clip) as f:
        for row in csv.reader(f):
            v = np.array(row[1:], dtype=np.float64)
            emb[key(row[0])] = v / np.linalg.norm(v)
    votes = defaultdict(list)
    with open(f"{a.eva}/data/votes_filtered.csv") as f:
        r = csv.reader(f, delimiter="=")
        next(r)
        for row in r:
            votes[row[0]].append(float(row[2]))
    ava = {}
    with open(a.ava) as f:
        for line in f:
            p = line.split()
            c = np.array(p[2:12], dtype=float)
            if c.sum() > 0:
                ava[p[1]] = float((c * np.arange(1, 11)).sum() / c.sum())
    mus = {key(k): v for k, v in read_musiq(a.eva_musiq).items()}
    ava_test = set(open(a.ava_test).read().split()) if a.ava_test else set()

    ids = sorted(i for i in votes if i in emb and len(votes[i]) >= 10 and i in ava)  # aesthetic.py's photos
    lost = [i for i in ids if i not in mus]
    assert not lost, f"{len(lost)} EVA photos have no MUSIQ score: {lost[:5]}"
    X = np.stack([emb[i] for i in ids])
    eva = np.array([np.mean(votes[i]) for i in ids])
    avam = np.array([ava[i] for i in ids])
    m_ava = np.array([mus[i]["ava"] for i in ids])
    held = np.array([i in ava_test for i in ids])
    print(f"\nEVA: {len(ids)} photos (aesthetic.py's selection), {held.sum()} of them in AVA's standard test list")

    def cv_predict(X, y, seed=0):  # aesthetic.py
        out = np.zeros(len(y))
        for tr, te in KFold(10, shuffle=True, random_state=seed).split(X):
            out[te] = RidgeCV(alphas=ALPHAS_A).fit(X[tr], y[tr]).predict(X[te])
        return out

    def cv_blend(X, extra, y, seed=0):
        """CLIP embedding plus MUSIQ score(s), z-scored on each training fold, one ridge."""
        out = np.zeros(len(y))
        extra = extra.reshape(len(y), -1)
        for tr, te in KFold(10, shuffle=True, random_state=seed).split(X):
            sc = StandardScaler().fit(extra[tr])
            Z = lambda idx: np.c_[X[idx], sc.transform(extra[idx])]
            out[te] = RidgeCV(alphas=ALPHAS_A).fit(Z(tr), y[tr]).predict(Z(te))
        return out

    def cv_calibrated(extra, y, seed=0):
        """MUSIQ alone, linearly recalibrated per fold (Spearman-neutral for one score)."""
        out = np.zeros(len(y))
        extra = extra.reshape(len(y), -1)
        for tr, te in KFold(10, shuffle=True, random_state=seed).split(extra):
            out[te] = make_pipeline(StandardScaler(), RidgeCV(alphas=ALPHAS_A)).fit(extra[tr], y[tr]).predict(extra[te])
        return out

    ours = cv_predict(X, eva)
    blend = cv_blend(X, m_ava, eva)
    D = "EVA (10-fold CV)"
    report(D, "archivis aesthetic (CLIP ridge head, 10-fold CV)", ours, eva, "VALIDATION.md: 0.783")
    report(D, "MUSIQ ava checkpoint (no refit)", m_ava, eva, "most photos likely in its AVA training set")
    extra_tech = [c for c in TECH if c in next(iter(mus.values()))]
    if extra_tech:
        for c in extra_tech:
            report(D, f"MUSIQ {c} checkpoint (no refit)", np.array([mus[i][c] for i in ids]), eva)
    report(D, "blend: CLIP head + MUSIQ ava (one ridge, 10-fold CV)", blend, eva)
    if extra_tech:
        allm = np.array([[mus[i][c] for c in ["ava"] + extra_tech] for i in ids])
        report(D, "blend: CLIP head + all MUSIQ scores (one ridge, 10-fold CV)", cv_blend(X, allm, eva), eva)
        report(D, "blend: MUSIQ scores only (linear, 10-fold CV)", cv_calibrated(allm, eva), eva)
    D2 = "EVA, photos in AVA's standard test list"
    report(D2, "archivis aesthetic (same CV predictions)", ours[held], eva[held])
    report(D2, "MUSIQ ava checkpoint (no refit)", m_ava[held], eva[held], "photos it did not train on")
    report(D2, "blend: CLIP head + MUSIQ ava (same CV predictions)", blend[held], eva[held])
    D3 = "EVA photos vs AVA's own panel"
    report(D3, "archivis aesthetic (fit on EVA votes)", ours, avam, "VALIDATION.md: 0.455")
    report(D3, "MUSIQ ava checkpoint", m_ava, avam, "trained on AVA labels: leakage")
    report(D3 + ", AVA test list only", "MUSIQ ava checkpoint", m_ava[held], avam[held])
    report(D3 + ", AVA test list only", "archivis aesthetic", ours[held], avam[held])

with open(os.path.join(a.out, "results.csv"), "w", newline="") as f:
    w = csv.DictWriter(f, fieldnames=list(results[0]))
    w.writeheader()
    w.writerows(results)

if a.speed and os.path.exists(a.speed):
    print("\nSpeed (speed.py)")
    for r in csv.DictReader(open(a.speed)):
        print("  " + ", ".join(f"{k}={v}" for k, v in r.items()))
print(f"\nwrote {os.path.join(a.out, 'results.csv')}")
