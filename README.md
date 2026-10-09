# Archivis

Face search, natural-language search and objective quality ranking for very
large personal photo archives (built for a ~20 TB collection). One Go binary,
everything runs locally, nothing leaves your machine.

* **Find people.** Every face is detected (SCRFD) and embedded (ArcFace,
  InsightFace *buffalo_l*). Upload a photo to find everyone in it across the
  archive, click any face to see that person elsewhere, name people once and
  Archivis labels the rest of their photos automatically. *Discover* surfaces
  frequently-photographed people you haven't named yet.
* **Search by description.** "protest at night in the rain", "kids in snow",
  "press conference" — CLIP (ViT-B/32) text-to-image search, plus "more like
  this photo". Describe what is visible; ambiguous single words can mislead
  ("Bombay" finds the city, "a black Bombay cat" finds the cat).
* **Rank by objective quality.** Every photo gets separately measured focus
  (with face focus for portraits), exposure/clipping, colour cast / white
  balance, noise and JPEG compression — each checked against photos with known
  applied defects — plus a technical score calibrated on 10,000 photos rated by
  people, and an aesthetic score fit to ~30 people's ratings of appeal per
  photo, which learns your own taste from ratings you give on photo pages.
  Filter ("sharp, well exposed, neutral colour, no heavy
  compression"), see warning badges on thumbnails, and sort
  ("worst first" for culling). The *Scores* page in the UI explains each
  number.
* **Journalistic triage.** Bursts/sequences with the best frame first,
  duplicate detection, zero-shot categories (protest, speech, sports, crowd,
  portrait, document/screenshot, …; editable), EXIF/camera/lens/date filters,
  export of picks as symlinks or copies (e.g. to import into Lightroom).
* **Built for scale.** Parallel, incremental and resumable indexing (Ctrl-C any
  time, rerun to continue). Moved/renamed files and byte-identical copies are
  recognised by content fingerprint and not re-analysed. RAW files are read via
  their embedded full-size JPEG previews (CR2, CR3, NEF, ARW, DNG, RAF, ORF,
  RW2, PEF, …); HEIC via `sips` on macOS (or libheif/ImageMagick/libvips).
  `prune` keeps entries on drives that look offline: a folder whose
  catalogued files are all missing and that is itself gone or empty is held
  back unless you pass `--all-missing`.

Your original files are only ever read, never modified.

## Install

Requires Go 1.24+ and a C compiler (Xcode command-line tools on macOS).
For a permanent setup on macOS or Ubuntu (background services, nightly
indexing, access from other devices, backups) follow **[DEPLOY.md](DEPLOY.md)**.

```sh
go build -o archivis ./cmd/archivis
sudo mkdir -p /usr/local/bin && sudo install -m 755 archivis /usr/local/bin/archivis   # on your PATH
archivis setup          # downloads ONNX Runtime + models (~800 MB) into ~/.archivis
```

(Without the install step, run it as `./archivis` from this folder instead.
Or skip the clone: `go install github.com/auteursoft/archivis/cmd/archivis@latest`
puts it in `$(go env GOPATH)/bin`.)

`setup` fetches ONNX Runtime 1.29 for your platform, InsightFace buffalo_l
(face detection + recognition) and OpenAI CLIP ViT-B/32 as ONNX. The
aesthetic model is built in. Every download, including the ONNX Runtime
archive, is checked against a pinned SHA-256 before it is used. Data lives in
`~/.archivis` (override with `--data DIR` or `ARCHIVIS_DATA`).

Prebuilt ONNX Runtime archives exist for Apple Silicon Macs, Linux x64/arm64
and Windows x64. Microsoft no longer ships one for Intel Macs: there, install
ONNX Runtime 1.29 yourself (for example `brew install onnxruntime`) and pass
`--ort-lib /path/to/libonnxruntime.dylib`, or run `setup --skip-runtime`.

Note: the InsightFace models are released for **non-commercial research use
only**. That fits a personal archive; check the licence before any commercial
use.

## Use

```sh
# Index (repeat with more folders/drives any time; only new/changed files are processed)
archivis index /Volumes/Archive1 /Volumes/Archive2 --exclude "Lightroom Backups"

# Browse, search and name people in the web UI
archivis serve          # → http://127.0.0.1:8088
```

From the terminal:

```sh
archivis search "firefighters at night" --min-focus 0.7
archivis search --face portrait-of-alice.jpg --format paths
archivis search --like reference.jpg --year 2019
archivis list --person "Alice Smith" --min-overall 70 --sort aesthetic --format csv > alice.csv
archivis list --sort worst --limit 500 --format paths        # culling candidates
archivis export --tag protest --min-focus 0.7 --min-exposure 0.8 --limit 300 ~/Desktop/protest-picks
archivis people list                                # everyone named so far
archivis people match                               # auto-label faces of named people
archivis people rename "Alice" "Alice Smith"
archivis people merge "Alice Smith" "A. Smith"      # fold the second into the first
archivis people delete "Unknown 3"                  # its faces become unlabelled
archivis aesthetic train   # learn your taste from ratings given in the web UI
archivis aesthetic models  # every aesthetic model used, and the current blend
archivis retag          # after editing ~/.archivis/categories.txt
archivis prune /Volumes/Archive1   # forget files deleted from that drive
archivis stats ; archivis errors
archivis inspect --previews ~/Desktop/check /Volumes/Card/DCIM   # what Archivis reads from your camera's files
```

## Performance and hardware

Measured on one CPU core for a 24 MP JPEG with 6 faces: decode 0.7 s, face
detection 0.3–0.5 s, ArcFace ≈0.33 s per face, CLIP 0.2 s, quality metrics
0.2 s. Indexing therefore runs at roughly **0.5 photos/s per core**, so
expect 4–6 photos/s on an 8–10 core machine. That is several days for a few
million photos: let it run overnight, interrupt freely, it resumes.

* `--provider auto` uses CoreML on macOS (Apple Neural Engine/GPU) and the CPU
  elsewhere; `--provider cuda` with `archivis setup --cuda` uses an NVIDIA GPU
  (CUDA 12 build; Linux and Windows x64).
  If an accelerator gives trouble, `--provider cpu` always works.
* Network drives: run the indexer on the machine with the disks if you can;
  JPEG decoding needs the whole file.
* Searching is brute-force and exact: about 0.12 s per million faces on 4
  cores. On a 1-million-photo test catalogue, browsing and filtering take
  0.06–0.4 s, text and face search 0.15–0.25 s (0.4–1 s for text search
  combined with filters such as a person, year or tag).
* The web UI keeps every embedding in memory: about 1.1 GB RAM per million
  photos + faces on top of 1.4 GB for the models (≈10 GB for 3M photos with 5M
  faces), and takes about 25 s per million to start. Indexing does not.
* Catalogue size: roughly 2 KB per photo plus 1 KB per face in SQLite, and
  ~40 KB of thumbnails per photo.

## How it works

```
cmd/archivis          CLI (setup, index, serve, search, list, export, people, …)
internal/imageio      decoding, EXIF, RAW preview extraction, orientation, resampling, pHash
internal/quality      focus / exposure / colour cast / noise / colourfulness metrics
internal/ml           ONNX Runtime: SCRFD, ArcFace (+alignment), CLIP (+BPE tokenizer), aesthetic head
internal/indexer      parallel incremental pipeline, thumbnails, zero-shot categories
internal/store        SQLite catalogue (photos, faces, people, tags)
internal/vindex       in-memory int8 vector index (cosine, multi-query)
internal/catalog      search, person auto-matching, people discovery
internal/web          local web UI (Go templates, no build step)
```

## How well it works

See **[VALIDATION.md](VALIDATION.md)**: every component measured against ground
truth (identity labels, human-drawn face boxes, human quality ratings, known
applied defects, class labels), with the defects those tests uncovered and how
they were fixed. Headlines: 99.65% on the LFW face-verification benchmark;
99.9% precision / 98.9% recall when naming people from a single confirmed
face among 5,400 strangers; a scripted "label every photo of this person" task
completed 10/10 times in 3.0 actions; technical score agrees with human
ratings at Spearman 0.63 on held-out photos; training on 20 of a person's
own ratings cuts the aesthetic score's error for that person by a fifth. Rerun everything with
`tools/validate/`.

Tests: `go test ./...`. The model tests also need the downloaded models and
the ONNX Runtime library; without them they are skipped:

```sh
ARCHIVIS_TEST_MODELS=~/.archivis/models ONNXRUNTIME_LIB=~/.archivis/lib/libonnxruntime.dylib go test ./internal/ml/
```

Add `ARCHIVIS_REQUIRE_MODELS=1` to make a missing model or library a failure
instead of a skip (CI does).

The earlier Python prototype lives in the `legacy/` folder of
[auteursoft/DoFISaC](https://github.com/auteursoft/DoFISaC) for reference.
