#!/usr/bin/env python3
"""Parity check: does Go's technical rating equal the shipped model applied
to the features the harness exports?

    check_rating.py internal/quality/rating_model.json QUALITY_CSV [TRAIN_QUALITY_CSV]

QUALITY_CSV is `validate quality` output, whose "rating" column is Go's
quality.Rate and whose feature columns are what fit_quality.py trains on.
Recomputing the rating in numpy from those columns must reproduce Go's
value; if a feature were exported differently from what the runtime feeds
the model (as cast_strength once was), the two would disagree.

With TRAIN_QUALITY_CSV it also checks that the model's stored feature means
match that training data, i.e. that refitting on it reproduces the model.
"""
import csv
import json
import sys

import numpy as np

model = json.load(open(sys.argv[1]))
feats = model["features"]


def features(path):
    rows = list(csv.DictReader(open(path)))
    missing = [f for f in feats if f not in rows[0]]
    if missing:
        sys.exit(f"{path} lacks model features {missing}; regenerate it with `validate quality`")
    X = np.array([[float(r[f]) for f in feats] for r in rows])
    for i, f in enumerate(feats):
        if f in model["log_features"]:
            X[:, i] = np.log1p(X[:, i])
    return rows, X


def predict(X):  # mirrors quality.RateMetrics
    x = (X - np.array(model["mean1"])) / np.array(model["scale1"])
    P = np.array(model["powers"])
    terms = np.prod(x[:, None, :] ** P[None, :, :], axis=2)
    v = model["intercept"] + ((terms - model["mean2"]) / model["scale2"]) @ np.array(model["coef"])
    return 100 * np.clip(v, 0, 1)


rows, X = features(sys.argv[2])
go = np.array([float(r["rating"]) for r in rows])
diff = np.abs(predict(X) - go)
# CSV values carry 6 significant digits, so allow a little rounding slack.
ok = diff.max() < 0.05
print(f"{len(rows)} photos: max |numpy(model, exported features) - Go rating| = {diff.max():.4f} "
      f"(median {np.median(diff):.5f}) -> {'OK' if ok else 'MISMATCH'}")

if len(sys.argv) > 3:
    _, Xt = features(sys.argv[3])
    rel = np.abs(Xt.mean(axis=0) - model["mean1"]) / np.array(model["scale1"])
    for f, r in zip(feats, rel):
        if r > 1e-3:
            print(f"  training mean of {f} differs from the model's by {r:.3f} sd")
    train_ok = rel.max() <= 1e-3
    print(f"training-data means {'match' if train_ok else 'DO NOT match'} the model "
          f"(largest gap {rel.max():.2e} sd)")
    ok = ok and train_ok
sys.exit(0 if ok else 1)
