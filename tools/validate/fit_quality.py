#!/usr/bin/env python3
"""Fit and evaluate technical-quality models on BIQ2021 (official train/test split).

    fit_quality.py train_quality.csv train_mos.csv test_quality.csv test_mos.csv [train_clip.csv test_clip.csv]
"""
import csv
import sys
from urllib.parse import unquote

import numpy as np
from scipy.optimize import nnls
from scipy.stats import pearsonr, spearmanr
from sklearn.ensemble import HistGradientBoostingRegressor
from sklearn.linear_model import RidgeCV
from sklearn.pipeline import make_pipeline
from sklearn.preprocessing import StandardScaler

# The rating model's inputs, exactly as quality.RateMetrics reads them. The
# colour input is lab_cast (neutral-pixel Lab magnitude), not the reported
# grey-edge cast_strength; check_rating.py verifies Go and this agree.
RAW = ["sharpness", "sharpness_global", "brightness", "highlights", "shadows", "dynamic_range", "contrast",
       "lab_cast", "colorfulness", "noise"]
SCORES = ["focus_score", "exposure_score", "color_score", "noise_score"]


def load(qpath, mpath):
    q = {unquote(r["image"]): r for r in csv.DictReader(open(qpath))}
    m = {r["Image Name"]: float(r["MOS"]) for r in csv.DictReader(open(mpath))}
    keys = sorted(k for k in m if k in q)
    return keys, q, np.array([m[k] for k in keys])


def feats(keys, q):
    X = []
    for k in keys:
        r = q[k]
        f = [float(r[c]) for c in RAW]
        f[0], f[1] = np.log1p(f[0]), np.log1p(f[1])  # sharpness spans decades
        f[9] = np.log1p(f[9])
        X.append(f)
    return np.array(X)


def report(name, pred, y):
    print(f"  {name:<46} SROCC {spearmanr(pred, y).statistic:.3f}   PLCC {pearsonr(pred, y).statistic:.3f}")


ktr, qtr, ytr = load(sys.argv[1], sys.argv[2])
kte, qte, yte = load(sys.argv[3], sys.argv[4])
print(f"BIQ2021: train {len(ktr)} / test {len(kte)} images; all numbers below are on the held-out test split")
report("current technical score (hand-set weights)", [float(qte[k]["technical"]) for k in kte], yte)
report("focus score alone", [float(qte[k]["focus_score"]) for k in kte], yte)

S_tr = np.array([[float(qtr[k][c]) for c in SCORES] for k in ktr])
S_te = np.array([[float(qte[k][c]) for c in SCORES] for k in kte])
w, _ = nnls(np.c_[S_tr, np.ones(len(S_tr))], ytr)
print("  NNLS weights on sub-scores:", dict(zip(SCORES + ["bias"], np.round(w, 3))))
report("re-weighted sub-scores (non-negative, fit on train)", S_te @ w[:-1] + w[-1], yte)

Xtr, Xte = feats(ktr, qtr), feats(kte, qte)
ridge = make_pipeline(StandardScaler(), RidgeCV(alphas=np.logspace(-3, 3, 13))).fit(Xtr, ytr)
report("linear model on raw measurements", ridge.predict(Xte), yte)
coef = ridge[-1].coef_ / ridge[0].scale_
print("  linear model (unscaled) coefficients:", dict(zip(RAW, np.round(coef, 4))), "intercept", round(float(ridge[-1].intercept_ - (ridge[0].mean_ / ridge[0].scale_) @ ridge[-1].coef_), 4))
import os
os.environ.setdefault("OMP_NUM_THREADS", "1")
gb = HistGradientBoostingRegressor(max_iter=300, learning_rate=0.05, random_state=0).fit(Xtr, ytr)
report("gradient boosting on raw measurements (ceiling)", gb.predict(Xte), yte)

if len(sys.argv) > 6 and not sys.argv[5].startswith("--"):
    def clip(path, keys):
        d = {}
        for r in csv.reader(open(path)):
            d[unquote(r[0])] = np.array(r[1:], dtype=np.float32)
        return np.array([d[k] for k in keys])
    Ctr, Cte = clip(sys.argv[5], ktr), clip(sys.argv[6], kte)
    cr = RidgeCV(alphas=np.logspace(-2, 4, 13)).fit(Ctr, ytr)
    report("linear probe on CLIP image embedding", cr.predict(Cte), yte)
    both_tr, both_te = np.c_[Ctr, StandardScaler().fit(Xtr).transform(Xtr)], None
    sc = StandardScaler().fit(Xtr)
    br = RidgeCV(alphas=np.logspace(-2, 4, 13)).fit(np.c_[Ctr, sc.transform(Xtr)], ytr)
    report("CLIP embedding + raw measurements", br.predict(np.c_[Cte, sc.transform(Xte)]), yte)
    np.save("clip_quality_head.npy", np.r_[cr.coef_, cr.intercept_])
    print(f"  CLIP head alpha={cr.alpha_}; weights saved to clip_quality_head.npy")


def export_quadratic(Xtr, ytr, path):
    """Fit the shipped rating model (quadratic ridge) and write its parameters."""
    import json
    from sklearn.preprocessing import PolynomialFeatures
    pipe = make_pipeline(StandardScaler(), PolynomialFeatures(2), StandardScaler(), RidgeCV(alphas=np.logspace(-2, 4, 13)))
    pipe.fit(Xtr, ytr)
    s1, pf, s2, rg = pipe.steps[0][1], pipe.steps[1][1], pipe.steps[2][1], pipe.steps[3][1]
    s2scale = np.where(s2.scale_ == 0, 1, s2.scale_)
    json.dump({
        "features": RAW, "log_features": ["sharpness", "sharpness_global", "noise"],
        "mean1": s1.mean_.tolist(), "scale1": s1.scale_.tolist(),
        "powers": pf.powers_.tolist(),
        "mean2": s2.mean_.tolist(), "scale2": s2scale.tolist(),
        "coef": rg.coef_.tolist(), "intercept": float(rg.intercept_), "alpha": float(rg.alpha_),
        "trained_on": "BIQ2021 train split (10,000 images, MOS 0-1)",
    }, open(path, "w"), indent=1)
    return pipe


if __name__ == "__main__" and "--export" in sys.argv:
    p = export_quadratic(Xtr, ytr, sys.argv[sys.argv.index("--export") + 1])
    report("exported quadratic model", p.predict(Xte), yte)
