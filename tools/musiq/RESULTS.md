# MUSIQ vs archivis: benchmark results

Google's MUSIQ (Ke et al., ICCV 2021) was compared with archivis's current
technical and aesthetic scores, with the released checkpoints and the
NumPy/PyTorch port in this directory. This is measurement only: no shipped
code changed.

Every number below is a Spearman rank correlation with human ratings, with a
95% percentile bootstrap CI over photos (2,000 resamples, as in VALIDATION.md).
"vs archivis" is a **paired** bootstrap: both scores are evaluated on the
same resampled photos, so it is the CI of the difference itself. Raw output:
`results/bench_output.txt`, `results/results.csv`.

## Summary

| Dataset | archivis today | MUSIQ alone | Best blend | Cost per photo (1 checkpoint) |
|---|---|---|---|---|
| KonIQ-10k test (correctness gate) | — | **0.945** [0.938, 0.950] (paper 0.916) | — | |
| BIQ2021 test, technical | **0.632** [0.602, 0.659] | koniq **0.755** [0.730, 0.777], +0.123 [+0.095, +0.149] | measurements + koniq: **0.777** [0.756, 0.799], +0.146 [+0.126, +0.166] | 0.81 CPU-s at 512 px, 4.0 at 1600 px |
| EVA, aesthetic (10-fold CV) | **0.782** [0.768, 0.795] | ava 0.525 [0.501, 0.549], −0.257 | CLIP + ava: **0.805** [0.792, 0.816], +0.022 [+0.017, +0.027] | same again for a second checkpoint |

## 1. Correctness gate: KonIQ-10k

The koniq checkpoint on KonIQ-10k's official test split (2,015 photos at
1024×768; split from the `official_split` column of the KonIQ metadata):

| | Spearman | Pearson |
|---|---|---|
| This port | **0.945** [0.938, 0.950] | 0.955 |
| MUSIQ paper | 0.916 | 0.928 |

The port comes in above the paper, not below it. A bug in the port would
lower the number, so the port is taken as correct. The extra 0.03 most likely
means the released checkpoint was trained on more of KonIQ than the paper's
train split, possibly including these test photos. That would not affect the
BIQ2021 results, since no checkpoint was trained on BIQ2021. The same thing
shows up with the AVA checkpoint (§3). JPEG decoding is Pillow's rather than
TensorFlow's.

**Input resolution matters.** The same photos shrunk to a 512 px long edge
(Pillow Lanczos, `score.py -long-edge 512`) score **0.866** [0.852, 0.879]
(`results/koniq_512.csv`). So scoring a downscaled working copy costs about
0.08 of agreement on KonIQ.

## 2. Technical quality: BIQ2021 test

The official split was used: 2,000 test photos, 30 raters each, all 512 px.
The blends were refit on the train split only (9,999 photos; one train photo,
`ss02 (1014).jpg`, cannot be downloaded from the dataset's repository). Every
blend uses archivis's own 10 measurements (`validate quality`, transforms as
in `fit_quality.py`) and the shipped quadratic-ridge recipe, or a linear
ridge where noted.

| Method | Spearman [95% CI] | Pearson | vs archivis rating [paired 95% CI] |
|---|---|---|---|
| archivis rating as shipped | **0.632** [0.602, 0.659] | 0.687 | — |
| archivis rating refit here (same recipe) | 0.632 [0.604, 0.660] | 0.686 | reproduces |
| archivis technical score as shipped | 0.627 [0.598, 0.656] | 0.678 | (VALIDATION.md 0.628, on 1,998 photos) |
| MUSIQ koniq, no refit | **0.755** [0.730, 0.777] | 0.783 | **+0.123** [+0.095, +0.149] |
| MUSIQ spaq, no refit | 0.655 [0.625, 0.683] | 0.708 | +0.023 [−0.003, +0.050] |
| MUSIQ paq2piq, no refit | 0.690 [0.663, 0.716] | 0.723 | +0.058 [+0.029, +0.086] |
| measurements + koniq (quadratic) | **0.777** [0.756, 0.799] | 0.807 | **+0.146** [+0.126, +0.166] |
| measurements + spaq (quadratic) | 0.714 [0.689, 0.737] | 0.771 | +0.082 [+0.063, +0.101] |
| measurements + paq2piq (quadratic) | 0.737 [0.713, 0.759] | 0.780 | +0.105 [+0.087, +0.123] |
| measurements + koniq (linear) | 0.765 [0.743, 0.787] | 0.791 | +0.134 [+0.110, +0.157] |
| measurements + spaq (linear) | 0.696 [0.667, 0.720] | 0.744 | +0.064 [+0.042, +0.085] |
| measurements + paq2piq (linear) | 0.723 [0.698, 0.745] | 0.759 | +0.091 [+0.071, +0.112] |
| koniq + spaq + paq2piq (linear) | 0.758 [0.734, 0.782] | 0.788 | +0.126 [+0.098, +0.151] |
| measurements + all three (quadratic) | 0.780 [0.758, 0.801] | 0.814 | +0.149 [+0.129, +0.170] |

For context, VALIDATION.md lists deep networks *trained on BIQ2021* at
0.67–0.79. MUSIQ koniq reaches 0.755 without ever seeing BIQ2021. Blended with
archivis's measurements it reaches 0.777, near the top of that range. Adding
spaq and paq2piq on top of koniq is not worth their cost (+0.003).

## 3. Aesthetics: EVA

These are `aesthetic.py`'s photos: 4,070 EVA photos with at least 10 votes
that are also in AVA.txt. The CLIP embeddings come from `validate clip`, and
the folds are the same 10-fold `KFold(shuffle, random_state=0)`. Blends add
the z-scored MUSIQ score(s) to the CLIP embedding in one ridge, refit inside
each fold.

| Method | Spearman [95% CI] | Pearson | vs archivis [paired 95% CI] |
|---|---|---|---|
| archivis aesthetic (CLIP ridge head, CV) | **0.782** [0.768, 0.795] | 0.793 | — (VALIDATION.md 0.783) |
| MUSIQ ava, no refit | 0.525 [0.501, 0.549] | 0.537 | −0.257 [−0.279, −0.234] |
| MUSIQ koniq, no refit | 0.311 [0.281, 0.339] | 0.382 | −0.471 |
| MUSIQ ava + koniq only (linear, CV) | 0.554 [0.532, 0.577] | 0.578 | −0.228 |
| CLIP head + MUSIQ ava | **0.805** [0.792, 0.816] | 0.814 | **+0.022** [+0.017, +0.027] |
| CLIP head + MUSIQ koniq | 0.795 [0.782, 0.808] | 0.809 | +0.013 [+0.009, +0.018] |
| CLIP head + MUSIQ ava + koniq | 0.810 [0.797, 0.821] | 0.821 | +0.027 [+0.022, +0.033] |
| *393 of the photos that are in AVA's standard test list:* | | | |
| archivis aesthetic (same CV predictions) | 0.770 [0.719, 0.812] | 0.773 | — |
| MUSIQ ava | 0.572 [0.506, 0.632] | 0.552 | −0.198 [−0.258, −0.137] |
| CLIP head + MUSIQ ava | 0.800 [0.758, 0.835] | 0.794 | +0.029 [+0.011, +0.049] |
| *Against AVA's own panel (the ava checkpoint's training labels):* | | | |
| archivis aesthetic | 0.455 [0.430, 0.478] | 0.460 | (VALIDATION.md 0.455) |
| MUSIQ ava, all 4,070 | 0.865 [0.856, 0.872] | 0.856 | |
| MUSIQ ava, the 393 test-list photos | 0.868 [0.837, 0.890] | 0.878 | (paper: 0.726 on AVA test) |

**Leakage caveat.** EVA's photos are AVA photos. The ava checkpoint matches
AVA's panel at 0.87 even on photos from AVA's standard test list, far above
the paper's 0.726, so the released checkpoint very likely trained on them
too. Its gain on EVA may therefore be inflated. The leak-free version is CLIP
plus the koniq score (+0.013). On its own, MUSIQ ava follows AVA's
dpchallenge panel and agrees with EVA's raters far less than archivis's
head does.

## 4. Speed

`speed.py` times the koniq checkpoint end to end: decode, multi-scale
preprocessing and the model. It is single-threaded process CPU time, averaged
over 12 KonIQ photos re-encoded at each size (JPEG q92), on this 4-core
machine. The 1600 px inputs are enlarged from 1024 px; MUSIQ's cost depends
only on pixel count. All checkpoints share one architecture, so each
additional checkpoint costs the same again (preprocessing is shared and
small).

| Long edge | Tokens | CPU s / photo | Wall s, one photo on 4 cores | 263,000 photos: CPU-hours | on 4 cores |
|---|---|---|---|---|---|
| 512 px | 385 | **0.81** | 0.28 | **59** | ~15 h |
| 1600 px | 2,093 | **4.0** | 1.26 | **292** | ~73 h |

These figures are for this PyTorch port. An ONNX export run through archivis's
ONNX Runtime would need its own measurement.

## Recommendation

**Adopt MUSIQ for the technical score, using the koniq checkpoint. Do not
adopt it for aesthetics.** For technical quality, koniq beats archivis's rating
by +0.12 out of the box. Blended with archivis's existing measurements
(refit on BIQ2021 train) the gain is +0.146 [+0.126, +0.166] (0.632 → 0.777).
That is the largest and cleanest gain here: no checkpoint saw BIQ2021, and
the paired CI is far from zero. spaq and paq2piq add nothing on top. The
cost is real: about 0.8 CPU-s per photo at 512 px (~59 CPU-hours for 263,000
photos) or 4 s at 1600 px (~290 CPU-hours). The KonIQ test shows accuracy
drops when photos are downscaled (0.945 → 0.866 at 512 px), so the analysis
resolution is a speed/accuracy trade-off to settle before shipping. BIQ2021's
own photos are only 512 px, so the 0.777 is already a 512 px figure. For
aesthetics, MUSIQ ava alone is much worse than the CLIP head (0.525 vs
0.782). Adding it to the head gains +0.022, but that costs a second
checkpoint's full price, and the gain is probably inflated because the
checkpoint appears to have trained on these AVA photos. If koniq ships for
technical, feeding its score into the aesthetic head is a free, leak-free
+0.013 [+0.009, +0.018] worth taking. Before shipping, check the licences of
the released checkpoints and their training data (the reference code is
Apache-2.0; KonIQ images carry individual CC licences), and verify an ONNX
export against this port.

## Reproducing

Datasets, kept outside the repo:

- BIQ2021 MOS CSVs and images: `raw.githubusercontent.com/nisarahmedrana/BIQ2021/main/` (`Images/<name>`).
- EVA: `raw.githubusercontent.com/kang-gnak/eva-dataset/master/` (`data/*.csv`, `images/EVA_together.zip.00[1-7]`).
- AVA.txt and `generic_test.jpgl`: `raw.githubusercontent.com/mtobeiyf/ava_downloader/master/AVA_dataset/`.
- KonIQ-10k 1024×768: the unmodified Hugging Face mirror `xyliu6/koniq10k-1024x768`. `datasets.vqa.mmsp-kn.de` was not reachable from this environment.
- KonIQ split and MOS: `meta_info_KonIQ10kDataset.csv` from the `chaofengc/IQA-PyTorch-Datasets-metainfo` dataset. This is a data file only; no pyiqa code was used.
- Checkpoints: `storage.googleapis.com/gresearch/musiq/{koniq,spaq,paq2piq,ava}_ckpt.npz`.
- CLIP ViT-B/32 ONNX from archivis's setup URLs, with SHA-256 matching `setup.go`. ONNX Runtime 1.29.0 came from the PyPI wheel's `libonnxruntime.so`, because the GitHub release was not reachable. This may explain EVA's 0.782 vs the documented 0.783.

```
go build -o validate ./tools/validate
./validate quality -out biq_test_quality.csv  BIQ/test     # and BIQ/train
ONNXRUNTIME_LIB=.../libonnxruntime.so.1.29.0 ./validate clip -models MODELS -out eva_clip.csv EVA/images/EVA_together
# repeat each until "0 to score"; output is appended and resumable
python3 tools/musiq/score.py -out koniq_test.csv -ckpt CKPT -models koniq -limit 700 KONIQ/test
python3 tools/musiq/score.py -out koniq_test_512.csv -ckpt CKPT -models koniq -long-edge 512 -limit 1100 KONIQ/test
python3 tools/musiq/score.py -out biq_test_musiq.csv  -ckpt CKPT -models koniq,spaq,paq2piq -limit 1000 BIQ/test
python3 tools/musiq/score.py -out biq_train_musiq.csv -ckpt CKPT -models koniq,spaq,paq2piq -limit 1000 BIQ/train
python3 tools/musiq/score.py -out eva_musiq.csv -ckpt CKPT -models ava,koniq -limit 1400 EVA/sel   # the 4,070 selected photos
python3 tools/musiq/speed.py -ckpt CKPT/koniq_ckpt.npz -out speed.csv -n 12 KONIQ/test/<12 photos>
python3 tools/musiq/bench.py -out OUT -koniq-meta meta_info_KonIQ10kDataset.csv -koniq-scores koniq_test.csv \
  -biq-train-q biq_train_quality.csv -biq-test-q biq_test_quality.csv \
  -biq-train-mos "Train (Images and MOS).csv" -biq-test-mos "Test (Images and MOS).csv" \
  -biq-musiq-train biq_train_musiq.csv -biq-musiq-test biq_test_musiq.csv \
  -eva EVA -ava AVA.txt -ava-test generic_test.jpgl -eva-clip eva_clip.csv -eva-musiq eva_musiq.csv -speed speed.csv
```

Scoring time on 4 cores: KonIQ test about 15 min per checkpoint. BIQ2021
about 80 min for all 12,000 photos with three checkpoints. EVA about 27 min
with two checkpoints.
