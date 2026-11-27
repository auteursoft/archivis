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

## Penalties

`quality.Technical` multiplies the rating by exposure, colour-cast and JPEG
penalties. Their strengths (exposure 0.1, colour 0.1, compression 0.2) were
tuned for today's rating; on `rating_musiq` they cost 0.028 agreement (above).
They were retuned for `rating_musiq` with the same tool and rule, on tuning
data only.

**Recommended for photos MUSIQ has scored: exposure 0.1, colour 0.0,
compression 0.1** (`musiqPenalties`, used by `quality.TechnicalWithMUSIQ`).
Photos without a MUSIQ score keep today's 0.1 / 0.1 / 0.2.

### Rule and tuning data

`tools/validate/tune_penalties.py --rating-col rating_musiq` on BIQ2021
**train** (9,999 photos) plus the **tuning half** of the ladder (80 photos):
over a 0.1-step grid (0 … 0.8 for each strength), the strengths with the
highest mean ladder AUC (over, under, warm, cool, green, JPEG; two strongest
levels each) among those that cost at most 0.01 Spearman on BIQ2021 train.

```
rating column: rating_musiq
BIQ2021 train SROCC without penalties: 0.778
chosen: exposure 0.1, colour 0.0, compression 0.1; BIQ train SROCC 0.772
tuning-ladder AUC (two strongest levels) before -> after:
  over   0.58 -> 0.71
  under  0.72 -> 0.75
  warm   0.48 -> 0.48
  cool   0.47 -> 0.46
  green  0.50 -> 0.49
  jpeg   0.61 -> 0.79
```

**Why colour is 0.** On BIQ2021 train, a colour penalty of 0.1 on its own
costs `rating_musiq` 0.014 Spearman (0.05: 0.0035; 0.2: 0.046). That is over
the 0.01 budget even with the other two strengths at 0. So at the grid's
0.1 step, no colour penalty fits. People, and MUSIQ, mostly do not mark warm
or tinted photos down. As before, colour casts are still reported by the
colour score and the cast badge.

**Sanity check: the same run on today's rating chooses today's strengths.**
`tune_penalties.py` (default `--rating-col rating`) on the same train data and
new tuning photos picks exposure 0.1, colour 0.1, compression 0.2, at a train
cost of 0.009 (0.644 → 0.635). That matches the shipped constants.

### Held-out results: BIQ2021 test (2,000 photos)

Spearman with 95% bootstrap CI (2,000 resamples of the photos,
`numpy.random.default_rng(0)`); paired CIs use the same resamples. AUC: the
worst quarter of photos by MOS against the best quarter.

| Score | Spearman [95% CI] | Pearson | Worst-vs-best AUC |
|---|---|---|---|
| `rating` (today, no MUSIQ) | 0.632 [0.602, 0.659] | 0.687 | 0.916 |
| technical shipped (`rating` × today's 0.1 / 0.1 / 0.2) | 0.627 [0.597, 0.655] | 0.678 | 0.911 |
| MUSIQ alone | 0.753 [0.729, 0.775] | 0.782 | 0.947 |
| `rating_musiq` | 0.777 [0.756, 0.796] | 0.807 | 0.964 |
| `rating_musiq` × today's strengths | 0.749 [0.725, 0.771] | 0.786 | 0.954 |
| **`rating_musiq` × new (0.1 / 0 / 0.1)** | **0.768** [0.746, 0.788] | 0.802 | 0.960 |

| Difference | Δ Spearman [paired 95% CI] |
|---|---|
| **new − today's strengths** (both on `rating_musiq`) | **+0.019** [+0.013, +0.025] |
| **new − technical shipped** | **+0.141** [+0.123, +0.159] |
| today's strengths on `rating_musiq` − technical shipped | +0.122 [+0.107, +0.139] |
| new − `rating_musiq` (cost of the new penalties) | −0.009 [−0.014, −0.003] |
| today's strengths on `rating_musiq` − `rating_musiq` | −0.028 [−0.036, −0.019] |
| new − MUSIQ alone | +0.015 [+0.002, +0.027] |
| technical shipped − `rating` (today's cost, sanity check) | −0.004 [−0.015, +0.007] |

The test split's numbers for `rating`, technical, MUSIQ and `rating_musiq`
equal the Results table above (rescored from scratch; a few CI bounds differ
by 0.001–0.002 from that table).

### Held-out results: ladder (73 photos)

AUC = how well the score separates the originals from the degraded versions
at that level (`roc_auc_score`, all originals against all versions): 1.0
always lower, 0.5 chance. This is the definition VALIDATION.md's table and
`tune_penalties.py` use. Mild → severe.

| Defect | technical shipped | `rating_musiq` | `rating_musiq` × today's | **`rating_musiq` × new** |
|---|---|---|---|---|
| Overexposure +0.5 / 1 / 1.5 / 2 EV | 0.51 / 0.51 / 0.52 / 0.55 | 0.52 / 0.54 / 0.56 / 0.60 | 0.55 / 0.59 / 0.63 / 0.68 | **0.57 / 0.63 / 0.68 / 0.74** |
| Underexposure −0.5 / 1 / 2 / 3 EV | 0.48 / 0.54 / 0.71 / 0.86 | 0.47 / 0.49 / 0.63 / 0.80 | 0.46 / 0.48 / 0.62 / 0.81 | **0.45 / 0.48 / 0.64 / 0.83** |
| Warm cast 5 / 10 / 20 / 35% | 0.51 / 0.53 / 0.56 / 0.59 | 0.49 / 0.48 / 0.48 / 0.47 | 0.50 / 0.53 / 0.58 / 0.64 | **0.49 / 0.48 / 0.48 / 0.47** |
| Cool cast | 0.49 / 0.50 / 0.51 / 0.56 | 0.50 / 0.50 / 0.48 / 0.49 | 0.49 / 0.50 / 0.52 / 0.61 | **0.50 / 0.50 / 0.46 / 0.47** |
| Green cast | 0.50 / 0.51 / 0.53 / 0.52 | 0.50 / 0.50 / 0.50 / 0.49 | 0.50 / 0.52 / 0.57 / 0.61 | **0.50 / 0.50 / 0.50 / 0.47** |
| JPEG quality 60 / 30 / 15 / 8 | 0.56 / 0.64 / 0.69 / 0.69 | 0.49 / 0.50 / 0.54 / 0.64 | 0.63 / 0.75 / 0.84 / 0.89 | **0.58 / 0.66 / 0.74 / 0.81** |
| Gaussian blur σ 0.5 / 1 / 2 / 4 px | 0.53 / 0.62 / 0.85 / 0.98 | 0.56 / 0.74 / 0.95 / 1.00 | 0.54 / 0.69 / 0.94 / 0.99 | **0.55 / 0.72 / 0.95 / 0.99** |
| Motion blur 3 / 7 / 15 / 31 px | 0.54 / 0.67 / 0.80 / 0.87 | 0.59 / 0.84 / 0.96 / 0.98 | 0.57 / 0.86 / 0.97 / 0.99 | **0.58 / 0.85 / 0.97 / 0.99** |
| Noise σ 2 / 4 / 8 / 16 | 0.52 / 0.57 / 0.67 / 0.77 | 0.53 / 0.57 / 0.65 / 0.77 | 0.49 / 0.52 / 0.59 / 0.71 | **0.51 / 0.54 / 0.63 / 0.75** |
| Haze (contrast −15 / 30 / 50 / 70%) | 0.46 / 0.52 / 0.68 / 0.88 | 0.46 / 0.48 / 0.55 / 0.69 | 0.44 / 0.45 / 0.51 / 0.65 | **0.43 / 0.45 / 0.53 / 0.70** |
| Blur σ 1 / 2 / 4 + noise σ 8 | 0.84 / 0.97 / 1.00 | 0.87 / 0.98 / 1.00 | 0.83 / 0.97 / 1.00 | **0.86 / 0.98 / 1.00** |

What the new strengths trade, against today's strengths on `rating_musiq`:
+0.019 agreement with people (held out), stronger over-exposure detection
(0.74 vs 0.68 at +2 EV) and slightly better blur and noise; weaker JPEG
detection (0.81 vs 0.89 at q8, still above `rating_musiq` alone and above
shipped technical's 0.69); and **no response to colour casts** (0.47 vs 0.61–0.64
at 35%). Against today's shipped technical, the new score detects
over-exposure, JPEG, blur and motion blur better; it detects colour casts
less (0.47 vs 0.52–0.59 at 35%; shipped technical's cast response was already
weak), and under-exposure, noise and haze slightly less. Those last three
come from `rating_musiq` itself, not the penalties.

### Sanity check: today's rating with today's strengths

VALIDATION.md's numbers, rebuilt on the new ladder photos:

| | VALIDATION.md | This rebuild |
|---|---|---|
| Spearman `rating` → technical (BIQ2021 test) | 0.632 → 0.628 | 0.632 → 0.627 (Δ −0.004 [−0.015, +0.007]) |
| Technical worst-vs-best quarter | 92% | 91.1% |
| Train cost of today's strengths | ≤ 0.01 | 0.009 |
| `rating` AUC at the worst level: over / casts | 0.56 / 0.54–0.59 | 0.45 / 0.42–0.46 |
| `rating` AUC at JPEG q60 | 0.30 | 0.44 (q8: 0.31) |
| Gaussian blur, focus score | 0.59 / 0.82 / 0.98 / 0.99 | 0.57 / 0.82 / 0.99 / 1.00 |
| Motion blur, focus | 0.62 / 0.74 / 0.82 / 0.85 | 0.59 / 0.70 / 0.79 / 0.83 |
| Noise, noise score | 0.64 / 0.99 / 1.00 / 1.00 | 0.63 / 0.96 / 1.00 / 1.00 |
| Overexposure, exposure | 0.67 / 0.83 / 0.91 / 0.94 | 0.71 / 0.83 / 0.89 / 0.93 |
| Underexposure, exposure | 0.48 / 0.55 / 0.83 / 0.96 | 0.40 / 0.46 / 0.75 / 0.90 |
| Warm cast, colour | 0.55 / 0.66 / 0.81 / 0.95 | 0.56 / 0.67 / 0.81 / 0.91 |
| Cool cast, colour | 0.55 / 0.60 / 0.76 / 0.91 | 0.48 / 0.55 / 0.72 / 0.86 |
| Green cast, colour | 0.56 / 0.65 / 0.79 / 0.90 | 0.52 / 0.60 / 0.76 / 0.87 |
| Blur + noise, focus | 0.81 / 0.97 / 0.98 | 0.81 / 0.98 / 0.99 |

The sub-scores reproduce within a few hundredths; the rating's own reaction
to over-exposure, casts and JPEG differs more, in both directions, between the
two photo sets. Both sets agree that today's rating barely reacts to these
defects, so the penalties are what makes technical react.

### Ladder photos

`tools/musiq/ladder_photos.txt` lists the 153 photos (150 Open Images
validation photos, CC BY 2.0, and three full-resolution camera JPEGs: Canon
EOS 5D Mark III, Nikon D750, Nikon D810), with URL, SHA-256, license and
split (80 tuning / 73 held out, by `numpy.random.default_rng(0)`). The
original 153 were not recorded, so this is a comparable rebuild, not the same
photos. Flickr (the Open Images originals) and Wikimedia Commons were not
reachable from this environment; the Open Images images came from the
dataset's own mirror on `open-images-dataset.s3.amazonaws.com`. `validate
ladder` resizes every input to at most 1600 px (its default `-max`), so the
camera JPEGs are measured at 1600 px, and MUSIQ and the ratings at 512 px as
always.

### Commands

Set-up as in Inputs above (same model, same SHA-256; ONNX Runtime from the
1.29.0 PyPI wheel, same library SHA-256; BIQ2021 from raw.githubusercontent.com,
9,999 / 2,000). The scores were measured at commit a41e689; rebuilt after the
no-fused-multiply-add preprocessing change (ca4a428), `validate` gives
identical `musiq`, `rating_musiq`, `rating` and `technical` values on 300 test
photos and 3 ladder photos (amd64).

```
go build -o validate ./tools/validate
# BIQ2021: one 2,000-photo chunk per command (directories of symlinks), then concatenated
ONNXRUNTIME_LIB=ORT/libonnxruntime.so ./validate quality -models MODELS -workers 4 -out biq_train_0.csv chunks/biq_train_0  # … _4
ONNXRUNTIME_LIB=ORT/libonnxruntime.so ./validate quality -models MODELS -workers 4 -out biq_test.csv chunks/biq_test_0
# ladder: 10 photos per command (about 2 minutes each), then concatenated per half
ONNXRUNTIME_LIB=ORT/libonnxruntime.so ./validate ladder -models MODELS -out lad_tune_00.csv chunks/lad_tune_00  # … _07
ONNXRUNTIME_LIB=ORT/libonnxruntime.so ./validate ladder -models MODELS -out lad_hold_00.csv chunks/lad_hold_00  # … _07

python3 tools/validate/tune_penalties.py --rating-col rating_musiq biq_train.csv train_mos.csv ladder_tune.csv
python3 tools/validate/tune_penalties.py biq_train.csv train_mos.csv ladder_tune.csv   # sanity check: 0.1 / 0.1 / 0.2
python3 tools/musiq/penalties_report.py biq_test.csv test_mos.csv ladder_hold.csv 0.1,0,0.1
```

`penalties_report.py` prints the held-out tables above. It computes each
penalised score from a CSV's `rating` or `rating_musiq`, `exposure_score`,
`color_score` and `blockiness` columns, with `quality.Technical`'s formula.
