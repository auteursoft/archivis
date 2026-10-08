#!/usr/bin/env python3
"""Turn raw validation CSVs (from `go run ./tools/validate ...`) into metrics.

    stats.py detect    det.csv boxes.csv pos.txt neg.txt
    stats.py lfw       pairs.csv
    stats.py autolabel autolabel.csv
    stats.py ladder    ladder.csv
    stats.py mos       quality.csv mos.csv
    stats.py text      text.csv
"""
import csv
import sys
from collections import defaultdict

import numpy as np


def rows(path):
    with open(path, newline="") as f:
        return list(csv.DictReader(f))


def iou(a, b):
    w = min(a[2], b[2]) - max(a[0], b[0])
    h = min(a[3], b[3]) - max(a[1], b[1])
    if w <= 0 or h <= 0:
        return 0.0
    i = w * h
    return i / ((a[2] - a[0]) * (a[3] - a[1]) + (b[2] - b[0]) * (b[3] - b[1]) - i)


def detect(det_csv, boxes_csv, pos_txt, neg_txt):
    pos = set(open(pos_txt).read().split())
    neg = set(open(neg_txt).read().split())
    gt = defaultdict(list)
    gtflags = defaultdict(list)
    for r in rows(boxes_csv):
        if r["LabelName"] == "/m/0dzct" and r["ImageID"] in pos:
            gt[r["ImageID"]].append((float(r["XMin"]), float(r["YMin"]), float(r["XMax"]), float(r["YMax"])))
            gtflags[r["ImageID"]].append((r["IsOccluded"], r["IsTruncated"]))
    det = defaultdict(list)
    size = {}
    for r in rows(det_csv):
        size[r["image"]] = (float(r["w"]), float(r["h"]))
        if r["x1"]:
            det[r["image"]].append((float(r["x1"]), float(r["y1"]), float(r["x2"]), float(r["y2"]), float(r["score"])))
    bins = [(0, 16), (16, 24), (24, 32), (32, 48), (48, 64), (64, 128), (128, 1e9)]
    hit = defaultdict(int)
    tot = defaultdict(int)
    fp_pos = 0
    ndet_pos = 0
    for img in pos:
        if img not in size:
            continue
        W, H = size[img]
        ds = det[img]
        used = set()
        for g in gt[img]:
            wpx = (g[2] - g[0]) * W
            b = next(i for i, (lo, hi) in enumerate(bins) if lo <= wpx < hi)
            tot[b] += 1
            best, bi = 0, -1
            for i, d in enumerate(ds):
                if i in used:
                    continue
                v = iou(g, d[:4])
                if v > best:
                    best, bi = v, i
            if best >= 0.3:
                used.add(bi)
                hit[b] += 1
        ndet_pos += len(ds)
        fp_pos += len(ds) - len(used)
    print("Face detection on Open Images V5 validation (human-drawn face boxes; IoU >= 0.3)")
    T = sum(tot.values())
    H_ = sum(hit.values())
    print(f"  images with faces: {len([i for i in pos if i in size])}, annotated faces: {T}")
    print(f"  overall recall: {H_}/{T} = {H_ / T:.3f}")
    for i, (lo, hi) in enumerate(bins):
        if tot[i]:
            label = f"{lo:.0f}-{hi:.0f}px" if hi < 1e8 else f">={lo:.0f}px"
            print(f"    face width {label:>10}: recall {hit[i] / tot[i]:.3f}  (n={tot[i]})")
    big = sum(tot[i] for i in range(3, len(bins)))
    bighit = sum(hit[i] for i in range(3, len(bins)))
    print(f"  recall for faces >= 32px: {bighit / big:.3f} (n={big})")
    # faces the annotators marked as neither occluded nor truncated
    clean_t = clean_h = 0
    for img in pos:
        if img not in size:
            continue
        W, H = size[img]
        for g, flags in zip(gt[img], gtflags[img]):
            if flags != ("0", "0") or (g[2] - g[0]) * W < 32:
                continue
            clean_t += 1
            if max([iou(g, d[:4]) for d in det[img]] + [0]) >= 0.3:
                clean_h += 1
    print(f"  recall for unoccluded, untruncated faces >= 32px: {clean_h / clean_t:.3f} (n={clean_t})")
    print(f"  detections not matching any annotated face (images with faces): {fp_pos}/{ndet_pos} = {fp_pos / max(1, ndet_pos):.3f}")
    nimg = [i for i in neg if i in size]
    fps = [len(det[i]) for i in nimg]
    print(f"  false detections on {len(nimg)} verified face-free images: {sum(fps)} total, "
          f"{sum(1 for x in fps if x) / len(nimg):.3f} of images affected")


def lfw(pairs_csv):
    from sklearn.metrics import roc_auc_score, roc_curve
    r = rows(pairs_csv)
    y = np.array([int(x["same"]) for x in r])
    s = np.array([float(x["score"]) for x in r])
    fold = np.array([int(x["fold"]) for x in r])
    miss = np.array([int(x["missing"]) for x in r])
    print("Face verification on LFW (official 6,000 pairs, full pipeline: detect -> align -> embed)")
    print(f"  pairs with an undetected face: {miss.sum()} (scored as non-match)")
    accs = []
    for k in range(10):
        tr, te = fold != k, fold == k
        cands = np.unique(s[tr])
        best = max(cands, key=lambda t: ((s[tr] >= t) == y[tr]).mean())
        accs.append(((s[te] >= best) == y[te]).mean())
    print(f"  10-fold accuracy: {np.mean(accs):.4f} ± {np.std(accs):.4f}")
    print(f"  ROC AUC: {roc_auc_score(y, s):.4f}")
    fpr, tpr, thr = roc_curve(y, s)
    for target in (1e-2, 1e-3):
        i = np.searchsorted(fpr, target, side="right") - 1
        print(f"  TAR @ FAR={target:g}: {tpr[i]:.4f} (threshold {thr[i]:.3f})")
    for t in (0.25, 0.35, 0.45, 0.5):
        same, diff = s[y == 1], s[y == 0]
        print(f"  at threshold {t:.2f}: same-person pairs accepted {np.mean(same >= t):.4f}, "
              f"different-person pairs accepted {np.mean(diff >= t):.4f}")
    print(f"  same-person similarity: median {np.median(s[(y == 1) & (miss == 0)]):.3f}, 5th pct {np.percentile(s[(y == 1) & (miss == 0)], 5):.3f}")
    print(f"  different-person similarity: median {np.median(s[(y == 0) & (miss == 0)]):.3f}, 99.9th pct {np.percentile(s[(y == 0) & (miss == 0)], 99.9):.3f}")


def autolabel(path):
    r = rows(path)
    auto = [x for x in r if x["source"] == "auto"]
    # Only central faces have a known identity. Auto labels on non-central
    # (background) faces are counted separately.
    auto_c = [x for x in auto if x["central"] == "1"]
    correct = sum(1 for x in auto_c if x["assigned"] == x["truth"])
    wrong_named = sum(1 for x in auto_c if x["assigned"] != x["truth"] and x["truth_named"] == "1")
    wrong_stranger = sum(1 for x in auto_c if x["truth_named"] == "0")
    bg = len(auto) - len(auto_c)
    targets = [x for x in r if x["central"] == "1" and x["truth_named"] == "1" and x["exemplar"] == "0"]
    named = len({x["truth"] for x in r if x["exemplar"] == "1"})
    k = sum(1 for x in r if x["exemplar"] == "1") // max(1, named)
    print(f"Auto-labelling simulation on LFW: {named} people named with {k} confirmed face(s) each; "
          f"all other {len({x['truth'] for x in r if x['truth']}) - named} LFW identities act as strangers")
    print(f"  faces to find (other photos of named people): {len(targets)}")
    print(f"  auto-labelled faces with known identity: {len(auto_c)}")
    print(f"    correct: {correct}   wrong person (named): {wrong_named}   stranger labelled as someone: {wrong_stranger}")
    p = correct / max(1, len(auto_c))
    rc = correct / max(1, len(targets))
    print(f"  precision: {p:.4f}   recall: {rc:.4f}")
    print(f"  auto labels on background (non-central, identity unknown) faces: {bg}")


def spearman(a, b):
    from scipy.stats import spearmanr
    return spearmanr(a, b).statistic


def ladder(path):
    from sklearn.metrics import roc_auc_score
    r = rows(path)
    by = defaultdict(dict)
    for x in r:
        by[x["image"]][(x["degradation"], float(x["level"]))] = x
    imgs = [i for i in by if ("none", 0.0) in by[i]]
    degs = []
    for x in r:
        if x["degradation"] != "none" and x["degradation"] not in degs:
            degs.append(x["degradation"])
    # which score should respond to which degradation
    target = {"blur": "focus_score", "motion": "focus_score", "noise": "noise_score", "over": "exposure_score",
              "under": "exposure_score", "warm": "color_score", "cool": "color_score", "green": "color_score",
              "flat": "exposure_score", "jpeg": "blockiness_inv", "blur+noise": "focus_score"}
    for i in by:  # higher = better convention for blockiness
        for k in by[i]:
            if "blockiness" in by[i][k]:
                by[i][k]["blockiness_inv"] = str(-float(by[i][k]["blockiness"]))
    scores = ["focus_score", "exposure_score", "color_score", "noise_score", "technical"]
    print(f"Quality metrics under controlled degradations ({len(imgs)} real photos, levels mild -> severe)")
    print("  For each degradation: the score that should respond, how often it ranks the levels in the right")
    print("  order (Spearman rho vs severity, mean over photos), and AUC for telling the degraded photo from")
    print("  its original at each level (1.0 = always, 0.5 = chance).")
    for d in degs:
        levels = sorted({k[1] for i in imgs for k in by[i] if k[0] == d})
        t = target[d]
        line = f"  {d:<11}"
        for sc in ([t] if t else []) + ["technical"]:
            rhos = []
            for i in imgs:
                vals = [float(by[i][("none", 0.0)][sc])] + [float(by[i][(d, lv)][sc]) for lv in levels if (d, lv) in by[i]]
                if len(set(vals)) > 1:
                    rhos.append(spearman(range(len(vals)), vals))
            aucs = []
            for lv in levels:
                a = [float(by[i][("none", 0.0)][sc]) for i in imgs]
                b = [float(by[i][(d, lv)][sc]) for i in imgs if (d, lv) in by[i]]
                aucs.append(roc_auc_score([1] * len(a) + [0] * len(b), a + b))
            line += f" | {sc}: rho {np.mean(rhos) if rhos else float('nan'):+.2f}, AUC " + " ".join(f"{v:.2f}" for v in aucs)
        print(line)
    print("  Cross-sensitivity: mean change of each score at the most severe level (should be ~0 off-target)")
    print("  " + " " * 12 + "".join(f"{s:>16}" for s in scores))
    for d in degs:
        levels = sorted({k[1] for i in imgs for k in by[i] if k[0] == d})
        lv = levels[-1]
        ch = []
        for sc in scores:
            diffs = [float(by[i][(d, lv)][sc]) - float(by[i][("none", 0.0)][sc]) for i in imgs if (d, lv) in by[i]]
            ch.append(np.mean(diffs) / (100 if sc in ("technical", "rating") else 1))
        print(f"  {d:<12}" + "".join(f"{v:>+16.2f}" for v in ch))
    # Baseline comparison: noise robustness of focus measure
    print("  Focus under noise (does noise fake sharpness?): fraction of photos where blur(sigma=2)+noise(8)")
    for sc in ("focus_score", "naive_lap"):
        worse = [float(by[i][("blur+noise", 2.0)][sc]) < float(by[i][("none", 0.0)][sc]) for i in imgs if ("blur+noise", 2.0) in by[i]]
        print(f"    scores lower than the sharp original with {sc}: {np.mean(worse):.3f}")


def mos(quality_csv, mos_csv):
    from scipy.stats import pearsonr, spearmanr
    from urllib.parse import unquote
    q = {unquote(x["image"]): x for x in rows(quality_csv)}
    m = {x["Image Name"]: float(x["MOS"]) for x in rows(mos_csv)}
    keys = [k for k in m if k in q]
    y = np.array([m[k] for k in keys])
    print(f"Agreement with human quality ratings: BIQ2021 test split ({len(keys)} real photos, MOS 0-1)")
    print("  Spearman (rank) and Pearson correlation of each score with mean opinion score:")
    for c in (["rating"] if "rating" in next(iter(q.values())) else []) + ["technical", "focus_score", "exposure_score", "color_score", "noise_score", "sharpness", "naive_lap", "brightness", "colorfulness", "contrast"]:
        x = np.array([float(q[k][c]) for k in keys])
        xs = np.log1p(x) if c in ("sharpness", "naive_lap") else x
        print(f"    {c:<15} SROCC {spearmanr(x, y).statistic:+.3f}   PLCC {pearsonr(xs, y).statistic:+.3f}")
    tech = np.array([float(q[k]["rating" if "rating" in q[k] else "technical"]) for k in keys])
    lo, hi = np.percentile(y, 25), np.percentile(y, 75)
    worst, best = y <= lo, y >= hi
    from sklearn.metrics import roc_auc_score
    print(f"  Separating the human-rated worst quarter from the best quarter by rating: AUC "
          f"{roc_auc_score(np.r_[np.ones(best.sum()), np.zeros(worst.sum())], np.r_[tech[best], tech[worst]]):.3f}")


def text(path):
    r = rows(path)
    by = defaultdict(list)
    for x in r:
        by[(x["label"], x["query"])].append(x)
    print("Text search on Imagenette validation (3,925 photos, 10 classes, ~390 per class)")
    p10s, p50s, aps = [], [], []
    for (label, q), hs in by.items():
        hs.sort(key=lambda x: int(x["rank"]))
        rel = [label in x["path"] for x in hs]
        p10, p50 = np.mean(rel[:10]), np.mean(rel[:50])
        hits = np.cumsum(rel)
        ap = np.sum([hits[i] / (i + 1) for i in range(len(rel)) if rel[i]]) / max(1, min(len(rel), 100))
        p10s.append(p10)
        p50s.append(p50)
        aps.append(ap)
        print(f"  {q!r:<52} P@10 {p10:.2f}  P@50 {p50:.2f}  P@100 {np.mean(rel[:100]):.2f}")
    print(f"  mean P@10 {np.mean(p10s):.3f}   mean P@50 {np.mean(p50s):.3f}   mean AP@100 {np.mean(aps):.3f}")


if __name__ == "__main__":
    cmd, args = sys.argv[1], sys.argv[2:]
    {"detect": detect, "lfw": lfw, "autolabel": autolabel, "ladder": ladder, "mos": mos, "text": text}[cmd](*args)
