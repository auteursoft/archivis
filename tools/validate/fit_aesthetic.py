#!/usr/bin/env python3
"""Fit archivis's aesthetic head: ridge regression from the L2-normalised
CLIP ViT-B/32 image embedding to the mean EVA rating (0-10, ~30 raters per
photo). EVA is CC0 (github.com/kang-gnak/eva-dataset).

    fit_aesthetic.py EVA_DIR CLIP_CSV OUT_JSON

CLIP_CSV is `validate clip` output for EVA_DIR/images/EVA_together.
aesthetic.py reports how well the result agrees with people.

A model's ID is its name plus a hash of its weights (ml.NewAesthetic), so a
refit with different weights always gets a new ID: catalogues register it as
a new model next to the old one and score photos with it from their stored
embeddings; the old model's scores are kept. After replacing
internal/ml/aesthetic_model.json, set ml.BuiltinAestheticID to the ID printed
here (TestDefaultAesthetic checks it).
"""
import hashlib
import struct
import csv
import json
import sys
from collections import defaultdict

import numpy as np
from sklearn.linear_model import RidgeCV

EVA, CLIP_CSV, OUT = sys.argv[1:4]
emb = {}
with open(CLIP_CSV) as f:
    for row in csv.reader(f):
        emb[row[0].rsplit(".", 1)[0]] = np.array(row[1:], dtype=np.float64)
votes = defaultdict(list)
with open(f"{EVA}/data/votes_filtered.csv") as f:
    r = csv.reader(f, delimiter="=")
    next(r)
    for row in r:
        votes[row[0]].append(float(row[2]))
ids = sorted(i for i in votes if i in emb and len(votes[i]) >= 10)
X = np.stack([emb[i] for i in ids])
y = np.array([np.mean(votes[i]) for i in ids])
m = RidgeCV(alphas=np.logspace(-2, 3, 12)).fit(X, y)
json.dump({
    "about": "Ridge regression from the L2-normalised CLIP ViT-B/32 image embedding to the mean "
             "EVA aesthetic rating (0-10). Fit by tools/validate/fit_aesthetic.py; see VALIDATION.md.",
    "name": "eva-ridge",
    "photos": len(ids),
    "alpha": m.alpha_,
    "w": [round(float(v), 7) for v in m.coef_],
    "b": round(float(m.intercept_), 7),
}, open(OUT, "w"), indent=None)
w = np.array([round(float(v), 7) for v in m.coef_] + [round(float(m.intercept_), 7)], dtype=np.float32)
digest = hashlib.sha256(b"".join(struct.pack("<f", v) for v in w)).hexdigest()[:12]
print(f"fit on {len(ids)} photos, alpha {m.alpha_:.3g}, wrote {OUT}: model ID eva-ridge-{digest}")
