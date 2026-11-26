# Fitting the MUSIQ technical rating (BIQ2021)

`internal/quality/rating_musiq_model.json` (`quality.RateWith`) is
archivis's 10 measurements plus MUSIQ koniq's score, run in Go through ONNX
Runtime at 512 px, in the same quadratic ridge as the shipped rating
(`fit_quality.py --export-musiq`). It was fit on the BIQ2021 train split only
and checked on the official 2,000-photo test split.

## Results (BIQ2021 test, 2,000 photos)

Spearman with 95% bootstrap CI (2,000 resamples, seed 0); paired CIs use the
same resamples for both scores.

| Score | Spearman [95% CI] | Pearson | vs `rating` [paired 95% CI] |
|---|---|---|---|
| `rating` (today's, no MUSIQ) | 0.632 [0.603, 0.659] | 0.687 | — |
| `technical` (today's, shipped) | 0.627 [0.598, 0.655] | 0.678 | |
| MUSIQ koniq alone (Go/ONNX) | 0.753 [0.730, 0.777] | 0.782 | |
| **`rating_musiq`** (this model) | **0.777** [0.755, 0.796] | 0.807 | **+0.145** [+0.127, +0.164] |
| technical with MUSIQ (`quality.Technical` on `rating_musiq`) | 0.749 [0.726, 0.771] | 0.786 | |

Other paired differences:

| Difference | Δ Spearman [paired 95% CI] |
|---|---|
| technical with MUSIQ − technical (shipped) | +0.122 [+0.107, +0.138] |
| `rating_musiq` − MUSIQ alone | +0.023 [+0.011, +0.035] |
| technical with MUSIQ − `rating_musiq` | −0.028 [−0.038, −0.018] |

Worst-vs-best quarter separation (AUC): technical 0.911, technical with
MUSIQ 0.954.

These match the PyTorch benchmark (`RESULTS.md`: koniq 0.755,
measurements + koniq 0.777, +0.146).

**Technical's penalties now cost more.** `quality.Technical`'s exposure,
colour and compression penalties were tuned to cost ≤ 0.01 agreement on
today's rating, and do so here (0.632 → 0.627). On `rating_musiq` they cost
0.028 [0.018, 0.038], which leaves technical with MUSIQ slightly below MUSIQ
alone. Each penalty on its own (test split, for diagnosis only): exposure
0.771, colour 0.765, compression 0.762. Penalties were not retuned here;
retuning belongs on tuning data (`tools/validate/tune_penalties.py`), not on
the test split.

`fit_quality.py`'s printed test numbers (from the first measurement run):

```
BIQ2021: train 9999 / test 2000 images; all numbers below are on the held-out test split
  current technical score (hand-set weights)     SROCC 0.627   PLCC 0.678
  focus score alone                              SROCC 0.442   PLCC 0.481
  re-weighted sub-scores (non-negative, fit on train) SROCC 0.460   PLCC 0.513
  linear model on raw measurements               SROCC 0.600   PLCC 0.641
  gradient boosting on raw measurements (ceiling) SROCC 0.668   PLCC 0.737
  MUSIQ alone (Go/ONNX)                          SROCC 0.753   PLCC 0.782
  exported quadratic model with MUSIQ            SROCC 0.777   PLCC 0.807
```

The ridge's chosen alpha is 0.01, the smallest in the grid (`logspace(-2, 4)`).

## Checks

- Every image gave a row: 9,999 train, 2,000 test, none with an empty `musiq`.
- After rebuilding with the fitted JSON, `validate quality -models` on test
  gives `rating_musiq` within 0.00013 points of the JSON model applied in
  Python to the exported features and `musiq` column, so Go and the fit agree.
- The `musiq` column is identical between the run before and after the fit.
- Technical with MUSIQ was computed as `rating_musiq × penalties`, the
  penalty factor taken from the `exposure_score`, `color_score` and
  `blockiness` columns. The same formula reproduces the `technical` column
  from `rating` to within 0.00016.
- `go test ./internal/quality ./internal/ml` passes;
  `ARCHIVIS_TEST_MODELS=… go test ./internal/ml -run MUSIQ` passes with the
  model below.

## Speed (CPU)

Intel Xeon @ 2.10 GHz, 4 cores, BIQ2021 images (512 px), `IntraOpThreads: 1`:

| Run | Per image |
|---|---|
| `validate quality -models`, 1 worker (200 photos) | 0.60 s wall, 0.61 CPU-s |
| `validate quality` without MUSIQ, 1 worker (200 photos) | 0.050 s wall |
| `validate quality -models`, 4 workers (2,000 test photos) | 0.146 s wall, 0.57 CPU-s |

So MUSIQ adds about 0.55 CPU-s per photo at 512 px.

## Inputs

- **Model**: `koniq.onnx` built with `tools/musiq/export_onnx.py` from
  `storage.googleapis.com/gresearch/musiq/koniq_ckpt.npz` (torch 2.15.0,
  onnx 1.23.2, onnxruntime 1.29.0). The release asset `musiq-koniq-1` was
  not published yet. The export's largest difference from the port on the
  test images is 3.05e-05. SHA-256
  `9873f28a7db80af9742bf35f5b48301d68e10770cb4ea188ffc36569bdf19c41`, the
  expected hash.
- **ONNX Runtime 1.29.0**: GitHub release downloads were not reachable from
  this environment, so `libonnxruntime.so.1.29.0` came from the PyPI wheel
  `onnxruntime-1.29.0-cp313-cp313-manylinux_2_28_x86_64.whl` (wheel SHA-256
  `e2128f31f449e922c62dbe5d8b6b7b079f0bcaf2d56a102fa203cb6e5bb5ab19`, library
  SHA-256 `8902e52a04bf6bf26f1613746811d6194f9c97ce7a6a7a28452b48522d3bd8d0`).
  The tarball's SHA-256 could not be checked.
- **BIQ2021**: `raw.githubusercontent.com/nisarahmedrana/BIQ2021/main/`
  (`Train (Images and MOS).csv`, `Test (Images and MOS).csv`,
  `Images/<name>`). Train is 9,999 photos because `ss02 (1014).jpg` returns
  404 upstream, the same photo `RESULTS.md` reports. No Hugging Face mirror
  was found. Test is complete (2,000).

## Commands

```
python3 tools/musiq/export_onnx.py koniq_ckpt.npz MODELS/musiq/koniq.onnx internal/ml/testdata/*.jpg
ARCHIVIS_TEST_MODELS=MODELS ONNXRUNTIME_LIB=ORT/libonnxruntime.so go test ./internal/ml -run MUSIQ

go build -o validate ./tools/validate
# one 2,000-photo chunk per command (directories of symlinks), then concatenated
ONNXRUNTIME_LIB=ORT/libonnxruntime.so ./validate quality -models MODELS -workers 4 -out train_aa.csv chunks/train_aa  # … train_ae
ONNXRUNTIME_LIB=ORT/libonnxruntime.so ./validate quality -models MODELS -workers 4 -out test_quality.csv chunks/test_aa

python3 tools/validate/fit_quality.py train_quality.csv train_mos.csv test_quality.csv test_mos.csv \
  --export-musiq internal/quality/rating_musiq_model.json

go build -o validate ./tools/validate   # fitted JSON embedded
ONNXRUNTIME_LIB=ORT/libonnxruntime.so ./validate quality -models MODELS -workers 4 -out test_quality_fitted.csv chunks/test_aa
```

`train_mos.csv`/`test_mos.csv` are BIQ2021's two MOS CSVs as published. The
JSON's `trained_on` was then edited by hand to say 9,999 images, since
`fit_quality.py` writes "10,000" regardless. The bootstrap statistics were
computed with a short NumPy/SciPy script over `test_quality_fitted.csv`
(`spearmanr`, `numpy.random.default_rng(0)`, 2,000 resamples of the 2,000
photos, 2.5th/97.5th percentiles).
