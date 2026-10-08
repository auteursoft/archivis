# Pilot checklist

The pilot tests Archivis on part of your real archive before you trust it with
the whole thing. Your photos are only read; the pilot catalogue lives in
`~/archivis-pilot` and can be deleted afterwards.

## 1. Automatic part (an evening, mostly waiting)

Pick one drive or folder of about 100,000–200,000 photos: ideally a mix of
years, cameras and phones, with some RAW and HEIC. Then:

```sh
cd ~/src/archivis && git pull && go build -o archivis ./cmd/archivis
ARCHIVIS=$PWD/archivis tools/pilot/pilot.sh /Volumes/Archive1/2015
# with an old photodex catalogue to check the upgrade (it works on a copy):
ARCHIVIS=$PWD/archivis tools/pilot/pilot.sh /Volumes/Archive1/2015 ~/.photodex
```

It indexes the folder, kills the indexer partway, as a power cut would
(`CRASH_AFTER`, default 10 minutes), then checks the database and finishes.
It also checks that an unchanged rerun analyses nothing, that copies are
recognised, which files failed and why, and how fast the web interface
answers. Send back `~/archivis-pilot/pilot-report.txt`. It lists the paths of
files that failed, so look through it first.

## 2. By hand (an hour or two)

Start the web interface on the pilot catalogue:

```sh
./archivis serve --data ~/archivis-pilot/data
```

Note what you find under each heading. Numbers are most useful, but a
sentence is fine.

**Photos**
- [ ] Are any photos sideways or upside down? Which camera or phone took them?
- [ ] Do RAW and HEIC photos show up, with the right camera, lens and date?
- [ ] Do any thumbnails look wrong (colours, cropping, blank)?

**People**
- [ ] Name 10–20 people you know well. For each, how many of the photos
      Archivis then finds automatically (marked "auto") are someone else?
- [ ] Is anyone obviously missing from photos where they clearly appear?
- [ ] Does *Discover* suggest real people, or mostly strangers and
      look-alikes?

**Search**
- [ ] Try 10 descriptions you would actually search for ("Mom at the beach",
      "protest signs", "birthday cake"). How many of the first 20 results are
      right?
- [ ] "More like this" on a few photos: sensible?

**Quality**
- [ ] Sort by *worst*: are those really your worst photos (blurred, dark,
      accidental)? Are good photos wrongly flagged "soft" or "exposure"?
- [ ] Sort by *overall*: do the top photos look like your best?

**Speed**
- [ ] Anything that felt slow, and roughly how long it took.

## 3. Taste trial (a few weeks, alongside normal use)

The aesthetic score starts from a crowd's taste and learns yours from your
ratings.

1. On photos you open anyway, rate them 1–10, or say the score is too low,
   about right or too high. Aim for 100–200 over a few weeks, across
   different kinds of photos.
2. Then see what training would do, without changing anything:
   ```sh
   ./archivis aesthetic train --data ~/archivis-pilot/data --dry-run
   ```
3. If it says the new model is better, train it for real (drop `--dry-run`),
   then sort by *aesthetic*. Is the order closer to yours than before? To
   undo it: `./archivis aesthetic use eva-ridge`.

Send back the output of `aesthetic train --dry-run` and
`aesthetic models`, and your impression of the before and after.

## Afterwards

```sh
rm -rf ~/archivis-pilot      # your photos are untouched
```
