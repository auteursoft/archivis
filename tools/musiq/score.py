#!/usr/bin/env python3
"""Score images with one or more MUSIQ checkpoints.

    score.py -out scores.csv -ckpt DIR [-models koniq,spaq,paq2piq,ava] [-workers 4] IMAGE_OR_DIR...

Writes image,<model>,... (one row per image, the file's base name). Each
image is decoded and preprocessed once and run through every checkpoint.
Workers run single-threaded so N workers use N cores.
"""
import argparse
import csv
import os
import sys
from multiprocessing import Pool

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

EXT = (".jpg", ".jpeg", ".png", ".bmp")
_models = {}


def _init(ckpt_dir, names):
    import torch
    torch.set_num_threads(1)
    from model import MUSIQ
    for n in names:
        _models[n] = MUSIQ(os.path.join(ckpt_dir, f"{n}_ckpt.npz"))


def _score(path):
    import preprocess as pp
    try:
        seq = pp.multiscale(pp.load(path))
    except Exception as e:  # unreadable image
        print(f"skip {path}: {e}", file=sys.stderr)
        return None
    return [os.path.basename(path)] + [f"{_models[n].score(seq):.6f}" for n in _models]


def images(args):
    out = []
    for a in args:
        if os.path.isdir(a):
            out += [os.path.join(a, f) for f in os.listdir(a) if f.lower().endswith(EXT)]
        else:
            out.append(a)
    return sorted(out)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("-out", required=True)
    ap.add_argument("-ckpt", required=True)
    ap.add_argument("-models", default="koniq,spaq,paq2piq")
    ap.add_argument("-workers", type=int, default=os.cpu_count())
    ap.add_argument("paths", nargs="+")
    a = ap.parse_args()
    names = a.models.split(",")
    paths = images(a.paths)
    done = set()
    if os.path.exists(a.out):  # resume
        with open(a.out) as f:
            done = {r["image"] for r in csv.DictReader(f)}
    todo = [p for p in paths if os.path.basename(p) not in done]
    print(f"{len(paths)} images, {len(todo)} to score with {names}", file=sys.stderr)
    new = not done
    with open(a.out, "a", newline="") as f, Pool(a.workers, _init, (a.ckpt, names)) as pool:
        w = csv.writer(f)
        if new:
            w.writerow(["image"] + names)
        for i, row in enumerate(pool.imap_unordered(_score, todo, chunksize=4)):
            if row:
                w.writerow(row)
            if i % 200 == 0:
                f.flush()
                print(f"{i}/{len(todo)}", file=sys.stderr, flush=True)


if __name__ == "__main__":
    main()
