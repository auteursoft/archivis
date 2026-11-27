"""MUSIQ (Ke et al., ICCV 2021) in PyTorch, ported from Google's reference
(google-research/musiq/model/{multiscale_transformer,
multiscale_transformer_utils,resnet}.py, Apache-2.0) and loading its
released .npz checkpoints.

Architecture of the released checkpoints (run_predict_image.py config):

  patch (32x32x3) -> weight-standardised 7x7/2 conv (64) -> GroupNorm -> ReLU
  -> 3x3/2 max pool -> one bottleneck unit (64 -> 256) -> flatten 8x8x256
  -> Dense 384
  + hashed spatial embedding (10x10 grid) + scale embedding (3 scales)
  [CLS] + 14 pre-norm transformer blocks (6 heads, MLP 1152, tanh GELU)
  -> LayerNorm -> CLS -> Dense(num_classes)

The AVA checkpoint predicts 10 logits; its score is the softmax-weighted mean
of 1..10. The others predict the mean opinion score directly.

Ported details: Flax 'SAME' padding is TF-style (asymmetric, extra pixel
after), so convs and the max pool are padded explicitly; old-Flax GroupNorm
uses 32 groups and eps 1e-6 (the root) or 1e-4 (residual unit); LayerNorm
eps 1e-6; conv kernels are standardised over (h, w, in) with eps 1e-5 added
to the std; patch features are flattened in (h, w, c) order; padded tokens
are masked out as attention keys.
"""
from __future__ import annotations

import math

import numpy as np
import torch
import torch.nn.functional as F

HIDDEN, HEADS, LAYERS = 384, 6, 14


def _same_pad(x, k, s, value=0.0):
    """TF 'SAME' padding for NCHW input."""
    h, w = x.shape[-2:]
    ph = max((math.ceil(h / s) - 1) * s + k - h, 0)
    pw = max((math.ceil(w / s) - 1) * s + k - w, 0)
    return F.pad(x, (pw // 2, pw - pw // 2, ph // 2, ph - ph // 2), value=value)


def _std_kernel(k):
    """(kh, kw, in, out) Flax kernel -> standardised torch (out, in, kh, kw)."""
    k = k.astype(np.float64)
    m = k.mean(axis=(0, 1, 2), keepdims=True)
    sd = k.std(axis=(0, 1, 2), keepdims=True)
    k = (k - m) / (sd + 1e-5)
    return torch.from_numpy(k.astype(np.float32).transpose(3, 2, 0, 1).copy())


class MUSIQ(torch.nn.Module):
    def __init__(self, npz_path: str):
        super().__init__()
        z = np.load(npz_path)
        p = {k[len("opt/target/"):]: z[k] for k in z.files if k.startswith("opt/target/")}
        t = lambda a: torch.from_numpy(np.ascontiguousarray(a, dtype=np.float32))
        self.num_classes = p["head/kernel"].shape[1]
        self.conv_root = _std_kernel(p["conv_root/kernel"])
        self.gn_root = (t(p["gn_root/scale"].ravel()), t(p["gn_root/bias"].ravel()))
        u = "block1/unit1/"
        self.unit = {n: _std_kernel(p[u + n + "/kernel"]) for n in ("conv1", "conv2", "conv3", "conv_proj")}
        self.unit_gn = {n: (t(p[u + n + "/scale"].ravel()), t(p[u + n + "/bias"].ravel()))
                        for n in ("gn1", "gn2", "gn3", "gn_proj")}
        self.emb_w, self.emb_b = t(p["embedding/kernel"]), t(p["embedding/bias"])
        T = "Transformer/"
        self.pos = t(p[T + "posembed_input/pos_embedding"][0])
        self.scale = t(p[T + "scaleembed_input/scale_embedding"][0])
        self.cls = t(p[T + "cls"]).reshape(1, 1, HIDDEN)
        self.blocks = []
        for i in range(LAYERS):
            b = f"{T}encoderblock_{i}/"
            a = b + "MultiHeadDotProductAttention_1/"
            self.blocks.append({
                "ln1": (t(p[b + "LayerNorm_0/scale"]), t(p[b + "LayerNorm_0/bias"])),
                "ln2": (t(p[b + "LayerNorm_2/scale"]), t(p[b + "LayerNorm_2/bias"])),
                # fused q|k|v projection: (384, 3*384)
                "qkv_w": t(np.concatenate([p[a + n + "/kernel"].reshape(HIDDEN, HIDDEN) for n in ("query", "key", "value")], 1)),
                "qkv_b": t(np.concatenate([p[a + n + "/bias"].reshape(HIDDEN) for n in ("query", "key", "value")])),
                "out_w": t(p[a + "out/kernel"].reshape(HIDDEN, HIDDEN)), "out_b": t(p[a + "out/bias"]),
                "fc1_w": t(p[b + "MlpBlock_3/Dense_0/kernel"]), "fc1_b": t(p[b + "MlpBlock_3/Dense_0/bias"]),
                "fc2_w": t(p[b + "MlpBlock_3/Dense_1/kernel"]), "fc2_b": t(p[b + "MlpBlock_3/Dense_1/bias"]),
            })
        self.norm = (t(p[T + "encoder_norm/scale"]), t(p[T + "encoder_norm/bias"]))
        self.head_w, self.head_b = t(p["head/kernel"]), t(p["head/bias"])

    # -- patch embedding ------------------------------------------------------
    @staticmethod
    def _gn(x, sb, eps):
        return F.group_norm(x, 32, sb[0], sb[1], eps)

    def _embed(self, px):
        """px: (N, 3072) patches -> (N, 384)."""
        n = px.shape[0]
        x = px.reshape(n, 32, 32, 3).permute(0, 3, 1, 2)
        x = F.conv2d(_same_pad(x, 7, 2), self.conv_root, stride=2)
        x = F.relu(self._gn(x, self.gn_root, 1e-6))
        x = F.max_pool2d(_same_pad(x, 3, 2, value=-math.inf), 3, 2)
        u, g = self.unit, self.unit_gn
        res = self._gn(F.conv2d(x, u["conv_proj"]), g["gn_proj"], 1e-4)
        y = F.relu(self._gn(F.conv2d(x, u["conv1"]), g["gn1"], 1e-4))
        y = F.relu(self._gn(F.conv2d(_same_pad(y, 3, 1), u["conv2"]), g["gn2"], 1e-4))
        y = self._gn(F.conv2d(y, u["conv3"]), g["gn3"], 1e-4)
        x = F.relu(res + y)
        x = x.permute(0, 2, 3, 1).reshape(n, -1)  # Flax flattens (h, w, c)
        return x @ self.emb_w + self.emb_b

    # -- transformer ----------------------------------------------------------
    def _block(self, x, keymask, b):
        B, L, D = x.shape
        h = F.layer_norm(x, (D,), *b["ln1"], eps=1e-6)
        q, k, v = (h @ b["qkv_w"] + b["qkv_b"]).split(D, dim=-1)
        q, k, v = (z.reshape(B, L, HEADS, D // HEADS).transpose(1, 2) for z in (q, k, v))
        a = F.scaled_dot_product_attention(q, k, v, attn_mask=keymask)
        x = x + a.transpose(1, 2).reshape(B, L, D) @ b["out_w"] + b["out_b"]
        h = F.layer_norm(x, (D,), *b["ln2"], eps=1e-6)
        h = F.gelu(h @ b["fc1_w"] + b["fc1_b"], approximate="tanh")
        return x + h @ b["fc2_w"] + b["fc2_b"]

    @torch.no_grad()
    def features(self, tokens):
        """tokens: (B, L, 3075) float tensor -> CLS representation (B, 384)."""
        B, L, _ = tokens.shape
        pos = tokens[..., -3].long()
        sc = tokens[..., -2].long()
        mask = tokens[..., -1] > 0.5
        x = self._embed(tokens[..., :-3].reshape(B * L, -1)).reshape(B, L, HIDDEN)
        x = x + self.pos[pos] + self.scale[sc]
        x = torch.cat([self.cls.expand(B, 1, HIDDEN), x], 1)
        mask = torch.cat([torch.ones(B, 1, dtype=torch.bool), mask], 1)
        keymask = mask[:, None, None, :]  # (B, 1, 1, L+1): which keys count
        for b in self.blocks:
            x = self._block(x, keymask, b)
        x = F.layer_norm(x, (HIDDEN,), *self.norm, eps=1e-6)
        return x[:, 0]

    @torch.no_grad()
    def forward(self, tokens):
        logits = self.features(tokens) @ self.head_w + self.head_b
        if self.num_classes == 1:
            return logits[:, 0]
        return (logits.softmax(-1) * torch.arange(1, self.num_classes + 1, dtype=torch.float32)).sum(-1)

    @torch.no_grad()
    def score(self, seq: np.ndarray) -> float:
        """Score one preprocessed sequence (preprocess.multiscale output)."""
        return float(self(torch.from_numpy(seq)[None])[0])
