"""MUSIQ multi-scale input, ported to NumPy from Google's reference
(google-research/musiq/model/preprocessing.py, Apache-2.0).

An image becomes one sequence of tokens. Each token is a 32x32 RGB patch
(flattened row, column, channel) followed by three numbers: its hashed
spatial position, its scale id and a mask bit. The sequence holds, in order:

  * the image resized (aspect ratio kept) to a 224 px longer side, scale id 0,
    padded with zero tokens to ceil(224/32)^2 = 49 tokens;
  * the same at 384 px, scale id 1, padded to 144 tokens;
  * the image at its own resolution, scale id 2, every patch, no padding.

Details that matter for matching the reference, each copied from it:

  * pixels go to [-1, 1] (x / 255 * 2 - 1) before resizing;
  * resizing is tf.image.resize(method=GAUSSIAN, antialias=False): TF's
    scale-and-translate with a Gaussian of radius 1.5 and sigma 0.5, weights
    normalised per output pixel, applied even when the size does not change;
  * the resized size is round(h * L / max(h, w)) with round-half-to-even;
  * patches come from tf.image.extract_patches(padding='SAME'), so an edge
    that is not a multiple of 32 is padded with zeros (mid-grey), the smaller
    half of the padding before the image;
  * the hashed position is row_hash * 10 + col_hash, where a row of n patches
    maps to grid cells min(floor(i * 10 / n), 9) (TF1 nearest-neighbour
    resize without half-pixel centres).

JPEG decoding here is Pillow's, not TensorFlow's; the two can differ by a
level or so per pixel.
"""
from __future__ import annotations

import numpy as np
from PIL import Image

PATCH = 32
GRID = 10
SCALES = (224, 384)


def load(path: str) -> np.ndarray:
    """Decode to float32 HxWx3 in [-1, 1] (EXIF orientation ignored, as TF does)."""
    with Image.open(path) as im:
        a = np.asarray(im.convert("RGB"), dtype=np.float32)
    return a / np.float32(255.0) * np.float32(2.0) - np.float32(1.0)


def from_uint8(rgb: np.ndarray) -> np.ndarray:
    return rgb.astype(np.float32) / np.float32(255.0) * np.float32(2.0) - np.float32(1.0)


def _gaussian(x):
    radius, sigma = 1.5, 0.5
    x = np.abs(x)
    return np.where(x >= radius, 0.0, np.exp(-x * x / (2.0 * sigma * sigma))).astype(np.float32)


def resize_weights(in_size: int, out_size: int) -> np.ndarray:
    """(out_size, in_size) matrix equal to TF's ScaleAndTranslate spans
    (Gaussian kernel, antialias=False, translation 0)."""
    scale = np.float32(out_size) / np.float32(in_size)
    inv_scale = np.float32(1.0) / scale
    radius = np.float32(1.5)
    W = np.zeros((out_size, in_size), dtype=np.float32)
    for x in range(out_size):
        sample = (np.float32(x) + np.float32(0.5)) * inv_scale
        if sample < 0 or sample > in_size:
            continue
        start = int(np.ceil(sample - radius - np.float32(0.5)))
        end = int(np.floor(sample + radius - np.float32(0.5)))
        start = min(max(start, 0), in_size - 1)
        end = min(max(end, 0), in_size - 1) + 1
        src = np.arange(start, end, dtype=np.float32)
        w = _gaussian(src + np.float32(0.5) - sample)
        s = w.sum(dtype=np.float32)
        if abs(s) >= 1000.0 * np.finfo(np.float32).tiny:
            w = w / s
        W[x, start:end] = w
    return W


def resize(img: np.ndarray, longer: int) -> np.ndarray:
    h, w = img.shape[:2]
    ratio = np.float32(longer) / np.float32(max(h, w))
    rh = int(np.round(np.float32(h) * ratio))  # numpy rounds half to even, like tf.round
    rw = int(np.round(np.float32(w) * ratio))
    Wh, Ww = resize_weights(h, rh), resize_weights(w, rw)
    c = img.shape[2]
    rows = (Wh @ img.reshape(h, w * c)).reshape(rh, w, c)  # vertical pass
    return np.ascontiguousarray(np.tensordot(rows, Ww, axes=([1], [1])).transpose(0, 2, 1), dtype=np.float32)


def _hash(n: int) -> np.ndarray:
    scale = np.float32(GRID) / np.float32(n)
    return np.minimum(np.floor(np.arange(n, dtype=np.float32) * scale), GRID - 1).astype(np.int64)


def patches(img: np.ndarray, scale_id: int, max_len: int | None) -> np.ndarray:
    """Tokens (n, 32*32*3 + 3) for one scale; max_len pads/cuts, None keeps all."""
    h, w, c = img.shape
    ch, cw = -(-h // PATCH), -(-w // PATCH)
    ph, pw = ch * PATCH - h, cw * PATCH - w
    img = np.pad(img, ((ph // 2, ph - ph // 2), (pw // 2, pw - pw // 2), (0, 0)))
    p = img.reshape(ch, PATCH, cw, PATCH, c).transpose(0, 2, 1, 3, 4).reshape(ch * cw, PATCH * PATCH * c)
    pos = (_hash(ch)[:, None] * GRID + _hash(cw)[None, :]).reshape(-1, 1).astype(np.float32)
    out = np.concatenate([p, pos, np.full_like(pos, scale_id), np.ones_like(pos)], axis=1)
    if max_len is not None:
        pad = np.zeros((max_len, out.shape[1]), dtype=np.float32)
        out = np.concatenate([out, pad])[:max_len]
    return out


def multiscale(img: np.ndarray, scales=SCALES, native: bool = True) -> np.ndarray:
    """The full MUSIQ input sequence (n_tokens, 3075) for an image in [-1, 1]."""
    parts = []
    for i, L in enumerate(sorted(scales)):
        parts.append(patches(resize(img, L), i, int(np.ceil(L / PATCH) ** 2)))
    if native:
        parts.append(patches(img, len(scales), None))
    return np.concatenate(parts)
