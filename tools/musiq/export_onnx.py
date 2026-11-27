#!/usr/bin/env python3
"""Export a MUSIQ checkpoint to ONNX for archivis (internal/ml/musiq.go).

    export_onnx.py CKPT.npz OUT.onnx [IMAGE...]

The graph takes one input, "tokens": (1, MAX_TOKENS, 3075) float32, the
sequence preprocess.multiscale builds for an image whose longer side is at
most 512 px (49 tokens at 224 px, 144 at 384 px, then up to 16x16 = 256 at the
image's own size), padded with zero tokens (mask 0) to MAX_TOKENS. Its one
output, "score", is (1,): the predicted mean opinion score. Padded tokens are
masked out of attention, so the padding does not change the score.

Given images, it checks the ONNX model against the PyTorch port on each, at
512 px, and fails if any score differs by more than 1e-3.

The released checkpoints come from Google Research's MUSIQ
(github.com/google-research/google-research/tree/master/musiq), Apache-2.0.
"""
import os
import sys

import numpy as np
import torch

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import preprocess as pp  # noqa: E402
from model import MUSIQ  # noqa: E402

EDGE = 512
MAX_TOKENS = 49 + 144 + (EDGE // pp.PATCH) ** 2  # 449


class Exportable(torch.nn.Module):
    def __init__(self, m: MUSIQ):
        super().__init__()
        self.m = m

    def forward(self, tokens):
        return self.m.forward(tokens)


def sequence(path: str) -> np.ndarray:
    from PIL import Image
    with Image.open(path) as im:
        im = im.convert("RGB")
        s = EDGE / max(im.size)
        if s < 1:
            im = im.resize((round(im.width * s), round(im.height * s)), Image.BILINEAR)
        img = pp.from_uint8(np.asarray(im))
    seq = pp.multiscale(img)
    pad = np.zeros((MAX_TOKENS - len(seq), seq.shape[1]), dtype=np.float32)
    return np.concatenate([seq, pad])


def main():
    ckpt, out, imgs = sys.argv[1], sys.argv[2], sys.argv[3:]
    m = MUSIQ(ckpt)
    dummy = torch.zeros(1, MAX_TOKENS, 32 * 32 * 3 + 3)
    dummy[0, :, -1] = 1
    torch.onnx.export(Exportable(m), (dummy,), out, input_names=["tokens"], output_names=["score"],
                      opset_version=17, dynamo=False, do_constant_folding=True)
    print(f"wrote {out} ({os.path.getsize(out) / 1e6:.1f} MB)")
    if not imgs:
        return
    import onnxruntime as ort
    sess = ort.InferenceSession(out, providers=["CPUExecutionProvider"])
    worst = 0.0
    for p in imgs:
        seq = sequence(p)
        n = int(np.nonzero(seq[:, -1] > 0.5)[0].max()) + 1  # up to the last real token
        want = m.score(seq[:n])  # the port, without the trailing padding
        got = float(sess.run(None, {"tokens": seq[None]})[0][0])
        worst = max(worst, abs(got - want))
        print(f"{os.path.basename(p)}: port {want:.5f} onnx {got:.5f} ({n} tokens)")
    print(f"largest difference {worst:.2e}")
    if worst > 1e-3:
        sys.exit("ONNX model disagrees with the port")


if __name__ == "__main__":
    main()
