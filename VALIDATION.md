# Archivis validation report

Every component was measured against **ground truth that Archivis did not
produce**: identity labels, human-drawn face boxes, human quality ratings, known
applied degradations, and class labels. Agreement with other software (e.g.
"matches InsightFace") was deliberately *not* used as evidence of correctness.
Where a test exposed a defect, the defect was fixed and the test re-run; both
the original and the final numbers are reported.

Everything here is reproducible with `tools/validate/` (Go harness writing raw
CSVs + Python statistics). Datasets were downloaded from their public sources;
none are redistributed in this repository.

Machine: 4-core Xeon VM, CPU only, often running several jobs at once, so all
timings are pessimistic.

## Summary

| Area | Test | Result |
|---|---|---|
| Face recognition | LFW, official 6,000 pairs, full pipeline | **99.65% ± 0.26%** accuracy (10-fold); 0 of 3,000 different-person pairs accepted at the product's 0.45 threshold |
| Naming people | 310 people named with 1 confirmed face each, 5,437 other identities as strangers (16,327 faces) | **precision 99.90%, recall 98.86%** (99.69% recall with 3 confirmed faces) |
| Usability: "label every photo of this person" | 10 random LFW people, real browser, real UI | **10/10 completed, 3.0 actions** on average, ~2 s per step; precision 98.0%, recall 99.2% |
| Face detection | Open Images V5, 500 photos, 929 human-drawn face boxes | recall **87.3%** overall, **92.9%** for unoccluded faces ≥ 32 px (was 85.0% / 90.6%); false detections on face-free photos 9.3% |
| Pets mistaken for people | 1,998 Oxford-IIIT Pets photos | **10.7%** counted as "with people" (was 31.9% with any face) |
| Technical quality vs people | BIQ2021 test split, 2,000 photos × 30 raters | Spearman **0.63** (was 0.43); separates worst from best quarter 92% of the time |
| Aesthetics vs people | EVA, 4,070 photos × ~30 raters (photos unseen by the model) | Spearman **0.78** (LAION predictor it replaced: 0.42); the raters' own panel reliability is 0.90 |
| Learning one person's taste | 25 EVA raters, their own held-out votes, 75 trials per row | error **1.81 → 1.46** points after 20 ratings; rank agreement **0.477 → 0.517** after 400; worse in < 1% of trials |
| Overall score vs people | EVA appeal ratings / BIQ2021 quality ratings | **0.71 / 0.71** (was 0.41 / 0.64) |
| Quality measurements vs known defects | 73 held-out photos × 11 degradations × 4 levels | blur σ≥2 AUC 0.98–0.99, noise σ≥4 0.99–1.00, +2 EV 0.94, −3 EV 0.96, casts 0.90–0.95, JPEG q≤30 0.97–1.00 |
| Text search | Imagenette (10 classes) / Oxford Pets (37 breeds) | mAP **0.985** / **0.757** |
| Robustness | corrupt / truncated / fake / bomb files, hard kill mid-run | all handled; memory bomb fixed (4.7 GB → rejected) |
| Accessibility | axe-core WCAG 2 A/AA on every page, light + dark; keyboard-only task | 0 violations (was 8 rule failures, 79 contrast nodes); keyboard upload fixed |
| Scale | synthetic catalogue, 1M photos / 1.05M faces | see [Scale](#scale) |

## Faces

### Recognition (LFW)

Labeled Faces in the Wild, 13,233 photos of 5,749 people, indexed through the
real `archivis index` pipeline (detection → landmark alignment → ArcFace
embedding). The official `pairs.txt` protocol: 3,000 same-person and 3,000
different-person pairs in 10 folds.

| | |
|---|---|
| 10-fold accuracy | **0.9965 ± 0.0026** |
| ROC AUC | 0.9957 |
| TAR @ FAR = 0.1% | 0.9933 |
| Same-person pairs accepted at 0.35 / 0.45 / 0.50 | 0.990 / 0.971 / 0.946 |
| Different-person pairs accepted at 0.35 / 0.45 / 0.50 | 0 / 0 / 0 |
| Pairs with an undetected face | 17 (18 of 13,233 photos had no detection) |

### Naming people at archive scale

3,000 negative pairs cannot show the error rate when each face is compared with
*millions* of strangers. So the product's own auto-labelling code
(`catalog.AutoMatch`, threshold 0.45, margin 0.08) was run on the whole LFW
catalogue: 310 people (everyone with ≥ 6 photos) were named from 1 (or 3)
randomly chosen confirmed faces, and every one of the other 5,437 identities
acted as a distractor.

| Confirmed faces per person | Faces to find | Precision | Recall | Wrong labels |
|---|---|---|---|---|
| 1 | 5,102 | **0.9990** | **0.9886** | 3 wrong person, 2 strangers named |
| 3 | 4,482 | **0.9989** | **0.9969** | 3 wrong person, 2 strangers named |

LFW only labels the centred face of each photo; ~180 auto-labels landed on
background faces whose identity is unknown, so they are excluded from the
counts above. A visual spot check of 24 suggests most are genuine
co-appearances (e.g. a named politician in another politician's photo).

### Face detection (Open Images)

500 randomly sampled Open Images V5 validation photos with human-drawn "Human
face" boxes (excluding photos with group boxes or depictions), plus 300 photos
verified to contain no human face. A detection matches a box at IoU ≥ 0.3.

| Variant | Recall (all) | Recall, faces ≥ 32 px | Unoccluded, untruncated ≥ 32 px | Face-free photos with a detection |
|---|---|---|---|---|
| SCRFD @ 640 (initial) | 0.850 | 0.863 | 0.906 | 9.0% |
| + close-up fallback | 0.873 | 0.888 | 0.920 | 11.0% |
| + rotation fallback | 0.882 | 0.898 | 0.933 | 13.7% |
| **+ fallbacks require score ≥ 0.7 (shipped)** | **0.873** | **0.888** | **0.929** | **9.3%** |
| SCRFD @ 1024 (rejected) | 0.797 | 0.799 | 0.855 | 7.3% |

Recall by face width: < 16 px 0.39, 16–24 px 0.82, 24–32 px 0.87,
≥ 32 px ~0.9.

*What the misses were* (inspected visually): faces filling the whole frame,
faces rotated by 90° or upside-down, backs of heads and profiles (counted as
faces by the annotators), and a CGI face. The two fallbacks — re-detecting on
a shrunken copy when nothing is found, and trying ±90° rotations for photos
without an EXIF orientation tag (old cameras, scans) — target the first two.
A larger detector input found more tiny faces but lost large ones, so it was
rejected.

*"False" detections*: of 48 randomly sampled detections that matched no
annotation on photos with faces, at least 43 were real, unannotated faces
(crowds, background people). On face-free photos the detections were almost
all **animal faces** (monkeys, apes, dogs, cats), carved or drawn faces, and two
blurs.

### Pets are not people

On 1,998 Oxford-IIIT Pets photos, the first version found a "face" in
**31.9%** — so the "photos with people" filter returned pets, and *Discover*
could propose dogs as people. Two hypotheses were tested against real human
faces (Open Images) and pet faces:

| Signal | Separates human from animal faces (AUC) |
|---|---|
| ArcFace embedding norm (a published face-quality signal) | 0.57 — rejected |
| Detector confidence | **0.92** |

Fix: detections from the fallback passes must score ≥ 0.7, and only faces
scoring ≥ 0.6 count as people (face counts, "with people", *Discover* uses
≥ 0.7). Weaker detections remain searchable. Result: pet photos with any
detection 31.9% → 19.2%; **counted as having people: 10.7%**; human-face
recall cost 0.9 points.

## Usability

### Task: "starting from one photo, label every photo of this person"

Driven through the real web UI with a headless browser (`ux_scenario.py`),
on the LFW catalogue, for 10 randomly chosen people with ≥ 15 photos. The
simulated user uploads the photo, types the name in the row of matches for
that face, presses **Assign**, then presses **Confirm selected** on the
person page if any suggestions are pre-ticked. The script scores the labels
only after the server has answered each request.

| | |
|---|---|
| Task completion | **10/10** |
| User actions | **3.0** on average (upload, type, Assign); auto-matching left nothing to confirm |
| Time per step (median) | upload + analysis 2.0 s, assign + auto-match 1.7 s |
| Precision / recall of the resulting labels | 0.980 / 0.992 |

All 5 labels counted as errors were *background* faces in other people's
photos — Mahmoud Abbas in a George W. Bush photo, Gray Davis with Barbara
Boxer, Renée Zellweger with Ewan McGregor, Saddam Hussein with Taha Yassin
Ramadan — i.e. very likely correct. Keyboard-only: 4/4 completed.

### Accessibility

axe-core 4.13 (WCAG 2 A, AA and best-practice rules) on all 13 page types, in
light and dark mode.

| | Before | After |
|---|---|---|
| Rule failures | 8 rules: unlabeled inputs (critical), 79 contrast failures, missing titles/landmarks on error pages, heading order | **0** |
| Dark-mode primary button contrast | 2.49:1 | 7.25:1 |
| Photo search reachable by keyboard | **no** (hidden file input) | yes (9 Tabs, Space) |

### Robustness

| Input | Result |
|---|---|
| Truncated JPEG, non-image named `.jpg`, random bytes named `.nef` | recorded as errors with clear messages; indexing continues |
| Bit-flipped JPEG | decodes and is indexed (Go decoder tolerates it) |
| Symlink loop, 0-byte file | skipped |
| PNG claiming 30000×30000 (2.6 MB file) | **was: 4.7 GB RAM in one worker.** Now rejected from the header (300 MP limit) |
| Process killed (`SIGKILL`) mid-index | re-run resumed: 2,791 photos skipped as unchanged, only in-flight work redone |
| Re-indexing changed every photo ID (broke links/bookmarks) | **fixed**: rows updated in place, IDs stable |
| Files < 16 KB silently skipped (e.g. LFW's 13–20 KB photos) | default lowered to 4 KB, `--min-bytes` flag added |
| Symlinked photo files silently skipped | now followed (links to folders still are not, to avoid loops) |
| *Discover* recomputed on every visit when no recurring faces were found | empty results now cached |
| Global flags before the command (`archivis --data X stats`) | was "unknown command"; now works |
| CLI misuse (missing args, bad values, unknown command) | actionable messages, non-zero exit codes |

Limitation: tests ran as root, so an unreadable-file case could not be
exercised.

## Photo quality

Two independent kinds of ground truth:

1. **Controlled degradations** of 153 real photos (Open Images, plus three
   12 MP photos): each defect applied at 4 known severities. Photos were split
   80 / 73: every calibration and design choice used only the first 80; the
   numbers below are from the **73 held-out photos**.
2. **Human ratings**: BIQ2021 (12,000 real photos, each rated by 30 people;
   official 10,000 / 2,000 train/test split). Models were fit on train only;
   numbers below are on **test**.

### Does each score detect its defect? (held-out photos)

AUC = how often the degraded photo scores worse than its original
(1.0 always, 0.5 chance), per severity, mild → severe.

| Defect | Score | AUC by severity | Mean change at worst level |
|---|---|---|---|
| Gaussian blur σ = 0.5 / 1 / 2 / 4 px | focus | 0.59 / 0.82 / 0.98 / 0.99 | −0.82 |
| Motion blur 3 / 7 / 15 / 31 px | focus | 0.62 / 0.74 / 0.82 / 0.85 | −0.46 |
| Noise σ = 2 / 4 / 8 / 16 | noise | 0.64 / 0.99 / 1.00 / 1.00 | −0.99 |
| Overexposure +0.5 / 1 / 1.5 / 2 EV | exposure | 0.67 / 0.83 / 0.91 / 0.94 | −0.66 |
| Underexposure −0.5 / 1 / 2 / 3 EV | exposure | 0.48 / 0.55 / 0.83 / 0.96 | −0.56 |
| Warm cast 5 / 10 / 20 / 35% | colour | 0.55 / 0.66 / 0.81 / 0.95 | −0.73 |
| Cool cast | colour | 0.55 / 0.60 / 0.76 / 0.91 | −0.65 |
| Green cast | colour | 0.56 / 0.65 / 0.79 / 0.90 | −0.63 |
| JPEG quality 60 / 30 / 15 / 8 | compression | 0.88 / 0.97 / 1.00 / 1.00 | — |
| Blur + noise σ 8 | focus | 0.81 / 0.97 / 0.98 | −0.77 |

Rule of thumb from these numbers: Archivis reliably flags blur of ≥ 2 px,
noise of σ ≥ 4, about 1.5 stops of over- or 2 stops of under-exposure, colour
shifts of ≥ 20%, and JPEG quality ≤ 30. Milder defects are detected only
partially, which is also roughly where they stop being obvious to the eye.

### Do scores respond *only* to their own defect? (held-out photos)

Mean change in each score at the most severe level. Off-diagonal values
should be near zero.

| Defect | focus | exposure | colour | noise |
|---|---|---|---|---|
| blur | **−0.82** | +0.01 | −0.10 | +0.01 |
| noise | +0.08 | +0.03 | +0.04 | **−0.99** |
| over | +0.05 | **−0.66** | +0.16 | −0.02 |
| under | −0.07 | **−0.56** | +0.02 | +0.01 |
| warm | 0.00 | +0.02 | **−0.73** | 0.00 |
| haze (−70% contrast) | −0.13 | −0.11 | +0.12 | +0.01 |

Noise does not masquerade as sharpness: a blurred (σ 2) *and* noisy photo
scores below its sharp original **98.6%** of the time with Archivis's focus
score, versus **23%** with the textbook Laplacian-variance measure.

### Agreement with people (BIQ2021 test, 2,000 photos)

| Score | Spearman | Pearson |
|---|---|---|
| **Technical (shipped)** | **0.628** | 0.678 |
| Human-calibrated rating alone | 0.632 | 0.687 |
| Original hand-weighted formula | 0.427 | 0.476 |
| Focus alone | 0.442 | 0.481 |
| For reference, published methods on the same split | | |
| — classical no-reference metrics (BRISQUE, NIQE, …) | 0.36–0.64 | |
| — deep networks trained on BIQ2021 | 0.67–0.79 | |

The technical score separates the photos people rated in the worst quarter
from those in the best quarter **92%** of the time.

The rating model is reproducible: `tools/validate/fit_quality.py` on the
harness output refits it identically (coefficients equal to 1e-15), and
`tools/validate/check_rating.py` confirms that Go's rating equals the model
applied to the exported features on all 1,998 test photos (max difference
0.0001 points). That check fails, by up to 14 points, if the model is fed the
reported grey-edge cast instead of its own colour input (`lab_cast`).

### What the testing changed

| Finding | Evidence | Fix |
|---|---|---|
| Noise score measured texture, not noise | correlated **−0.31** with human ratings; 0.80 with sharpness | estimate from the flattest 10% of the frame; now −0.09 and 0.22 |
| Hand-weighted technical score diluted its best signal | 0.43 vs 0.50 for focus alone | replaced by a model fit to 10,000 human-rated photos (0.63 held out) |
| Cool colour casts barely detected | AUC 0.68 at a 35% shift | grey-edge illuminant estimate: 0.91 (warm 0.86 → 0.95) |
| Hazy or dark photos read as out of focus | focus −0.43 under haze | contrast-normalised focus: −0.13 |
| Heavy JPEG compression invisible; mild compression *raised* scores | rating AUC 0.30 at q60 | blockiness measure at native resolution: 0.88–1.00 |
| A tried "improvement" made things worse | iterative neutral-pixel cast estimator: more false casts, no gain | reverted |

### Warning badges

Thumbnails carry badges for clear defects. Thresholds were set so that badges
rarely fire on untouched photos (held-out set):

| Badge | Shown when | Untouched photos flagged | Defect caught |
|---|---|---|---|
| soft | focus < 0.3 | 4.1% | 96% of photos blurred σ = 2 px |
| exposure | exposure < 0.5 | 5.5% | 69% of +2 EV / −3 EV |
| cast | colour < 0.1 | 9.6% | 74% of 35% casts |
| JPEG | blockiness > 1.5 | — | most JPEG quality ≤ 30 |

The cast badge started at colour < 0.5 and flagged 26% of untouched photos —
many genuinely tinted (indoor light), many just warm scenes; it was tightened
to strong casts only.

### An honest limitation: one number cannot be both

The human-calibrated rating barely reacts to over-exposure or colour casts
(AUC 0.56 / 0.54–0.59 at the worst level). Forcing it to — by penalising
those sub-scores — makes it disagree with people: at penalty strength 0.5 its
agreement with human ratings drops from 0.64 to 0.53 (tuning data). People
often do not consider a warm sunset or a bright, airy photo flawed. So the
single **technical** score follows people (with only mild penalties that cost
≤ 0.01 agreement), while **focus, exposure, colour cast and compression are
reported as separate scores and warning badges** — use those filters when you
want the objective check regardless of taste.

Caveats: BIQ2021 images are 512 px, so the rating model measures every photo
at 512 px; the controlled-degradation photos are ≤ 1024 px (the three 12 MP
ones aside). Absolute focus thresholds for 24–50 MP originals viewed at 100%
have not been validated against human judgement.

## Aesthetics

The aesthetic score is the one subjective component, so it was checked
against three sets of human ratings that Archivis did not produce:

* **EVA** (Kang, Valenzise & Dufaux 2020, CC0): 4,070 photographs drawn from
  the AVA photo-contest collection, each rated 0–10 for appeal by about 30
  people (median 33).
* **AVA** means for the same photos: about 200 dpchallenge.com voters each,
  an independent panel.
* **BIQ2021** test photos (everyday photos rated for quality), which the
  aesthetic model never sees.

### The LAION predictor did not hold up

Archivis originally used the LAION aesthetic predictor (`sa_0_4`, a linear head
on CLIP). Once its weights were reachable it was tested, and it fell well
short of the information in the CLIP features it reads:

| | LAION predictor | Linear head refit on EVA (10-fold CV) |
|---|---|---|
| vs EVA raters | 0.418 | **0.783** |
| vs AVA panel | 0.141 | **0.455** |
| best vs worst quarter ranked correctly | 79% | **98%** |

A likely reason: that checkpoint was trained largely on ratings of
AI-generated images. Archivis now ships a ridge-regression head fit to EVA's
ratings on the same CLIP embedding (`internal/ml/aesthetic_model.json`, fit
by `tools/validate/fit_aesthetic.py`). It is built in, so setup no longer
downloads anything for aesthetics.

### Is the refit real, or fit to one panel's taste?

All EVA numbers below are for photos the model did not see in training.

| Check | Archivis | LAION | People, for comparison |
|---|---|---|---|
| EVA raters | **0.783** [0.769, 0.796] | 0.418 | two halves of the panel agree at 0.818; full-panel reliability 0.900 |
| Fit on one half of the raters, tested on the other half | **0.740** | — | 0.818 |
| AVA's independent panel | **0.455** [0.429, 0.481] | 0.141 | EVA's panel vs AVA's: 0.493 |
| Content categories left out of training (6 categories) | **0.60–0.81** | 0.24–0.48 | |
| BIQ2021 everyday photos (quality ratings) | **0.642** | 0.466 | technical score: 0.628 |

It generalises to raters, content and photographs it was not fit on, and it
nearly matches how well one panel of people agrees with another. It is not
fragile: 250 training photos already reach 0.70 on held-out photos, and 3,600
reach 0.77.

Two things to keep in mind. First, AVA's own panel and EVA's agree only at
0.49 on these photos, so "aesthetic" is genuinely a matter of taste; treat the
score as a second opinion. Second, on everyday photos the aesthetic score also
tracks technical quality (0.64 with quality ratings): people find sharp, clean
photos more appealing, and the model learned that.

### Scale and the overall score

The aesthetic score is the predicted average rating out of 10. Most contest
photos land between 4.8 and 7.4 and most everyday photos between 4.2 and 7.1.
For the overall score it is mapped linearly, 3 → 0 and 8 → 100, which clips
under 1% of photos. The weight was chosen to serve both kinds of rating:

| Aesthetic weight in overall | vs EVA appeal ratings | vs BIQ2021 quality ratings |
|---|---|---|
| 0 (technical only) | 0.174 | 0.628 |
| 0.35 | 0.602 | 0.710 |
| **0.5 (shipped)** | **0.705** | **0.713** |
| 0.65 | 0.758 | 0.701 |
| 1 (aesthetic only) | 0.783 | 0.642 |
| before: 0.35 × LAION | 0.414 | 0.635 |

### Learning one person's taste

The score shown is what a crowd finds appealing. A person can correct it on
any photo page (too low / about right / too high, or their own 1–10 rating),
and `archivis aesthetic train` fits a model to those judgements. EVA
records every rater's individual votes, and some raters cast thousands, so
this was tested on real individual taste. `validate personal` simulates
each of the 25 most prolific raters as a user (`go run ./tools/validate
personal -eva EVA -clip eva_clip.csv -n 20,50,100,200,400`):

* EVA's photos are split in two for each rater. A stand-in for the built-in
  model is fit to the *other* raters' mean ratings on half A, so it has
  seen neither the test photos nor this rater.
* *n* of the rater's votes on half B are their feedback. The model is
  trained exactly as the app does it, and the rest of their votes on half B
  (at least 200 photos, median about 1,000) are the test.
* Feedback is either the ratings themselves, or only verdicts on the score
  shown: "too high" or "too low" when the rating is more than a point away,
  otherwise "about right".
* 25 raters × 3 random draws = 75 trials per row. "Deployed" is what the
  user ends up with: the new model if training judged it better, otherwise
  the unchanged score.

| Feedback | Judgements | New model used | Rank agreement with the person (median) | Error (points, median) | Ranking improved |
|---|---|---|---|---|---|
| ratings | 20 | 72% | 0.468 → 0.468 | 1.81 → **1.46** | 52% |
| ratings | 50 | 80% | 0.471 → 0.473 | 1.81 → **1.44** | 79% |
| ratings | 100 | 89% | 0.463 → **0.481** | 1.82 → **1.42** | 87% |
| ratings | 200 | 96% | 0.464 → **0.488** | 1.81 → **1.38** | 95% |
| ratings | 400 | 95% | 0.477 → **0.517** | 1.80 → **1.36** | 95% |
| verdicts | 20 | 72% | 0.468 → 0.468 | 1.81 → **1.63** | 49% |
| verdicts | 100 | 91% | 0.463 → 0.463 | 1.82 → **1.55** | 81% |
| verdicts | 400 | 96% | 0.477 → **0.491** | 1.80 → **1.54** | 96% |

* **A few judgements fix the scale.** Individuals use the 0–10 scale very
  differently: one of these raters averages almost two points below the
  crowd. Twenty judgements remove a fifth of the error.
* **Ranking needs more.** It improves steadily from about 100 ratings
  (+0.025 rank agreement on average at 100 ratings, +0.064 at 400). Verdicts
  carry less information than ratings but still help.
* **Rarely harmful.** Across all 750 trials, the deployed score ranked a
  person's photos worse by more than 0.01 in 5 trials (at most −0.06), and
  was less accurate in 4.

Early versions were not this safe, and the testing changed three things.
At 20 ratings, cross-validation picked the weakest pull toward the prior.
Its error estimates from four or five held-out points are mostly luck, so
it overfitted: rank agreement fell (0.51 → 0.44) and one rater got a worse
model. Training now:

* takes the strongest pull within one standard error of the best (the
  standard "one-standard-error rule");
* lets the bias move almost freely while the weights stay tied to the
  prior, so a person's scale is learnt without disturbing the ranking;
* adds a model to the blend only when its paired held-out gain exceeds its
  standard error.

### Model history and blending

Each model's ID is its name plus a hash of its weights
(`eva-ridge-828e586c6724` for the built-in model), so different weights can
never share an ID. A catalogue keeps every model it has used and every
model's score for every photo. The score shown is a weighted blend of the
active models, with weights chosen on held-out judgements.

In these trials the chosen blend nearly always gave the new model all the
weight (mix 1 in 98% of activations). This is expected: the new model
already starts from the current blend and is pulled toward it, so blending
the two again only adds the same pull twice. Blending earns its keep with
models that err differently, such as models fit to different raters or
datasets, or on different embeddings. Their stored scores make that cheap
to try later. Superseded models stay in the catalogue with all their
scores, and `archivis aesthetic use` brings any of them back.

Nothing is recomputed when a catalogue is opened. Scores written before
models were tracked are attributed to the model that made them: EVA
scores to `eva-ridge-828e586c6724`, older LAION scores to `laion-legacy`
(kept, not blended). This took 7 s on a 1M-photo synthetic catalogue. A new
model scores only the photos it has not scored, from their stored CLIP
embeddings: 27 s per million photos, with no re-indexing.

## Text search

Photos were indexed through `archivis index` and queried through the
product's own search path (`catalog.SearchText`).

| Benchmark | Queries | Result |
|---|---|---|
| Imagenette validation (3,925 photos, 10 everyday classes) | 14 (10 descriptive, 4 one-word) | P@10 **1.00**, P@50 0.999, full-ranking mAP **0.985**, R-precision 0.953 |
| Oxford-IIIT Pets (1,998 photos, 37 cat/dog breeds) | "a photo of a ⟨breed⟩, a type of pet" | P@10 **0.87**, mAP **0.757** (chance 0.027) |

Coarse subjects are found essentially perfectly; fine distinctions (one dog
breed versus a similar one) are found well but not perfectly — 8–9 of the
top 10 results are right. The worst case was instructive: "Bombay" (a black
cat breed) scored AP 0.03 because CLIP reads it as the city. Ambiguous words
need context ("a black Bombay cat").

## Scale

A synthetic catalogue with **1,000,000 photos and 1,054,458 faces** (random
embeddings, realistic metadata) was generated and every page timed through
HTTP (median of 5, idle 4-core VM). The first measurements found three pages
far too slow; the causes and fixes are below.

| Page | First version | Final |
|---|---|---|
| Browse / sort / page 50 | 64–75 ms | 56–62 ms |
| Filtered browse (focus + groups + cast) | 847 ms | **272 ms** |
| Category filter | 720 ms | 387 ms |
| Text search (with and without filters) | 660–710 ms* | **236–254 ms** |
| Photo page (incl. similar photos) / face search | 340–360 ms* | 129 / 145 ms |
| Person page (incl. suggestions) | **20 s** | **0.8 s** |
| Discover (first visit / cached) | **13 s every visit** | 4.8 s / 2 ms |
| Bursts | 6.6 s | 1.9–2.2 s (worst case: no bursts at all) |

\* measured under CPU contention.

| Cause | Fix |
|---|---|
| Person suggestions compared every face with up to 64 exemplars (64 full scans) | 8 mutually dissimilar exemplars |
| Discover compared each face with every cluster (quadratic) and never cached empty results | parallel comparison, 5,000-face cap, cache fixed |
| Filters and counts scanned rows carrying 1 KB embeddings (~3 GB) | embeddings moved to their own table (automatic one-off upgrade: 79 s for 1M photos) |
| Bursts scanned the whole table without a suitable index | newest-first scan with early stop + covering index |

Resource use at 1M photos + 1M faces: start-up 52 s, **3.8–4.6 GB RAM**
(1.4 GB of it is the models), 3.3 GB database. Memory and start-up grow
roughly linearly with the number of embeddings (~1.1 GB and ~25 s per million
photos + faces), so an archive of 3M photos with 5M faces needs about 10 GB of
RAM and ~3–4 minutes to start the web UI. Indexing itself does not hold the
vectors in memory.

## Not validated / known limitations

* **Real camera RAW and HEIC files** — RAW preview extraction was tested on
  constructed TIFF-structured files and the fallback scanner, not on files
  from real cameras; HEIC conversion was not exercised (no converter here).
* **macOS / CoreML** — all tests ran on Linux CPU.
* **Aesthetics of documentary work** — EVA's photos are contest entries
  rated by general viewers. How well "appeal" matches news value or a
  picture editor's judgement has not been measured. Training on an editor's
  own judgements is the remedy, but it was tested only with EVA raters
  standing in for users.
* **Verdict targets** — "too high" / "too low" are trained as the score
  shown ∓ 1 point at half weight, a heuristic. It helps (see above) but is
  not tuned.
* **Absolute focus thresholds at full resolution** — calibrated on ≤ 1024 px
  photos and 512 px human-rated photos.
* **Very small faces** — recall below 16 px is 39%; crowd shots will have
  missed faces.
* **Unreadable files** — tests ran as root.
