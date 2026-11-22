#!/usr/bin/env python3
"""CPU cost of one MUSIQ checkpoint per photo.

    speed.py -ckpt koniq_ckpt.npz -out speed.csv [-n 12] [-photos 263000] IMAGE...

Each photo is resized (Pillow, Lanczos) so its long edge is 512 px or
1600 px (archivis's analysis size), saved as JPEG q92 and then timed end to
end: decode, multi-scale preprocessing and the model, single-threaded
(process CPU time), plus wall time with all cores on one photo. MUSIQ
processes the photo at its own resolution plus 224 and 384 px, so the cost
grows with pixel count (patches) and roughly with its square (attention).
"""
import argparse
import csv
import io
import os
import sys
import time

import numpy as np
import torch
from PIL import Image

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import preprocess as pp  # noqa: E402
from model import MUSIQ  # noqa: E402

ap = argparse.ArgumentParser()
ap.add_argument("-ckpt", required=True)
ap.add_argument("-out", required=True)
ap.add_argument("-n", type=int, default=12)
ap.add_argument("-photos", type=int, default=263000)
ap.add_argument("paths", nargs="+")
a = ap.parse_args()
m = MUSIQ(a.ckpt)
cores = os.cpu_count()


def jpeg_at(path, edge):
    im = Image.open(path).convert("RGB")
    s = edge / max(im.size)
    im = im.resize((round(im.width * s), round(im.height * s)), Image.LANCZOS)
    b = io.BytesIO()
    im.save(b, "JPEG", quality=92)
    return b.getvalue(), im.size


def run(data):
    seq = pp.multiscale(pp.load(io.BytesIO(data)))
    m.score(seq)
    return seq.shape[0]


rows = []
for edge in (512, 1600):
    files = [jpeg_at(p, edge) for p in a.paths[: a.n]]
    torch.set_num_threads(1)
    run(files[0][0])  # warm-up
    cpu, tokens = [], []
    for data, _ in files:
        t = time.process_time()
        tokens.append(run(data))
        cpu.append(time.process_time() - t)
    torch.set_num_threads(cores)
    wall = []
    for data, _ in files[:4]:
        t = time.perf_counter()
        run(data)
        wall.append(time.perf_counter() - t)
    c = float(np.mean(cpu))
    rows.append(dict(long_edge=edge, size=f"{files[0][1][0]}x{files[0][1][1]}", tokens=int(np.mean(tokens)),
                     cpu_s_per_photo=round(c, 3), wall_s_one_photo_all_cores=round(float(np.mean(wall)), 3),
                     cores=cores, photos=a.photos, cpu_hours=round(c * a.photos / 3600, 1),
                     wall_hours_all_cores=round(c * a.photos / 3600 / cores, 1)))
    print(rows[-1], flush=True)

with open(a.out, "w", newline="") as f:
    w = csv.DictWriter(f, fieldnames=list(rows[0]))
    w.writeheader()
    w.writerows(rows)
