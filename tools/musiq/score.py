#!/usr/bin/env python3
"""Score images with one or more MUSIQ checkpoints.

    score.py -out scores.csv -ckpt DIR [-models koniq,spaq,paq2piq,ava] [-workers 4] [-limit N] [-long-edge PX] IMAGE_OR_DIR...

Writes image,<model>,... (one row per image, the file's base name). Each
image is decoded and preprocessed once and run through every checkpoint.
Rows are flushed as they are written and images already in the output are
skipped, so an interrupted run resumes; -limit N scores at most N more images
per invocation (run it repeatedly until "0 to score"). -long-edge PX first
shrinks each image (Pillow Lanczos, never enlarging) so its longer side is PX,
as archivis would before analysis; the default scores the file as stored.
Workers run single-threaded so N workers use N cores.
"""
import argparse
import csv
import os
import sys
from multiprocessing import Pool

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

EXT = (".jpg", ".jpeg", ".png", ".bmp")
_models = {}
_edge = 0


def _init(ckpt_dir, names, edge=0):
    global _edge
    _edge = edge
    import torch
    torch.set_num_threads(1)
    from model import MUSIQ
    for n in names:
        _models[n] = MUSIQ(os.path.join(ckpt_dir, f"{n}_ckpt.npz"))


def _score(path):
    import preprocess as pp
    try:
        if _edge:
            from PIL import Image
            with Image.open(path) as im:
                im = im.convert("RGB")
                s = _edge / max(im.size)
                if s < 1:
                    im = im.resize((round(im.width * s), round(im.height * s)), Image.LANCZOS)
                img = pp.from_uint8(np.asarray(im))
        else:
            img = pp.load(path)
        seq = pp.multiscale(img)
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
    ap.add_argument("-limit", type=int, default=0)
    ap.add_argument("-long-edge", type=int, default=0)
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
    if a.limit:
        todo = todo[: a.limit]
    if not todo:
        return
    new = not os.path.exists(a.out) or os.path.getsize(a.out) == 0
    with open(a.out, "a", newline="") as f, Pool(a.workers, _init, (a.ckpt, names, a.long_edge)) as pool:
        w = csv.writer(f)
        if new:
            w.writerow(["image"] + names)
        for i, row in enumerate(pool.imap_unordered(_score, todo, chunksize=4)):
            if row:
                w.writerow(row)
                f.flush()
            if i % 200 == 0:
                print(f"{i}/{len(todo)}", file=sys.stderr, flush=True)


if __name__ == "__main__":
    main()
