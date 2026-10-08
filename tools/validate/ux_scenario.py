#!/usr/bin/env python3
"""Task-based usability test of the people workflow, driven through the real
web UI with a headless browser, scored against LFW identities.

Task given to the simulated user: "Starting from one photo of PERSON, label
every photo of them in the archive."
  1. upload the photo (Search by photo)
  2. type the person's name in the row of matches for their face, press Assign
     (faces with similarity >= 0.5 are pre-ticked; Assign also auto-labels)
  3. on the person page, press "Confirm selected" once for the pre-ticked
     suggestions
After steps 2 and 3 the catalogue is checked against ground truth.

    ux_scenario.py BASE_URL DB_PATH LFW_ROOT [n_people] [--keyboard] --reset-labels

Uses Playwright's managed Chromium (`playwright install chromium`); set
CHROMIUM_PATH to use another Chrome/Chromium binary.

The test starts from a clean slate, so it DELETES every person and face label
in DB_PATH. Run it only against a disposable copy of a catalogue (the server
at BASE_URL must serve that copy); --reset-labels confirms that.
"""
import os
import random
import sqlite3
import statistics
import sys
import time

from playwright.sync_api import sync_playwright

BASE, DB, ROOT = sys.argv[1], sys.argv[2], os.path.abspath(sys.argv[3])
N = int(sys.argv[4]) if len(sys.argv) > 4 and sys.argv[4].isdigit() else 10
KEYBOARD = "--keyboard" in sys.argv
if "--reset-labels" not in sys.argv:
    sys.exit("refusing to run: this test deletes every person and face label in "
             f"{DB}.\nPoint it at a disposable copy of a catalogue and pass --reset-labels.")


def central_faces(con):
    best = {}
    for path, fid, x1, y1, x2, y2 in con.execute(
            "SELECT p.path, f.id, f.x1, f.y1, f.x2, f.y2 FROM faces f JOIN photos p ON p.id = f.photo_id"):
        d = ((x1 + x2) / 2 - .5) ** 2 + ((y1 + y2) / 2 - .5) ** 2
        if path not in best or d < best[path][0]:
            best[path] = (d, fid)
    return {p: v[1] for p, v in best.items()}


def truth_sets(con):
    cf = central_faces(con)
    by = {}
    for p, fid in cf.items():
        name = os.path.relpath(p, ROOT).split(os.sep)[0]
        by.setdefault(name, set()).add(fid)
    return cf, by


def assign_failed(resp):
    """Why an /api/assign response means the task did not complete, or None.
    auto_error means the typed label was saved but finding the rest of the
    person's photos failed -- the task ("label every photo") is incomplete."""
    if not resp.ok:
        return f"HTTP {resp.status} {resp.text()[:200]}"
    err = resp.json().get("auto_error")
    return f"automatic matching failed: {err}" if err else None


def score(con, name, truth):
    labelled = {r[0] for r in con.execute(
        "SELECT f.id FROM faces f JOIN people p ON p.id = f.person_id WHERE p.name = ?", (name,))}
    # identity is only known for central faces; others count as errors
    tp = len(labelled & truth)
    return tp / max(1, len(labelled)), tp / len(truth), len(labelled)


con = sqlite3.connect(DB)
con.execute("UPDATE faces SET person_id = NULL, person_source = NULL, person_sim = NULL")
con.execute("DELETE FROM people")
con.commit()
cf, by = truth_sets(con)
eligible = sorted(n for n, s in by.items() if len(s) >= 15)
if not eligible:
    sys.exit(f"no LFW identity has >= 15 indexed photos in {DB}; index LFW_ROOT into this catalogue first")
random.seed(11)
people = random.sample(eligible, min(N, len(eligible)))
print(f"{len(people)} people sampled from {len(eligible)} LFW identities with >= 15 photos; "
      f"{'keyboard only' if KEYBOARD else 'mouse'}")

results = []
with sync_playwright() as p:
    # Playwright's managed Chromium by default; CHROMIUM_PATH overrides it.
    b = p.chromium.launch(executable_path=os.environ.get("CHROMIUM_PATH") or None)
    pg = b.new_page(viewport={"width": 1400, "height": 900})
    for name in people:
        truth = by[name]
        # Query with a photo whose face was actually detected and indexed
        # (the identity is eligible through its other photos even if the
        # detector missed the first one). Images only: macOS adds .DS_Store.
        files = sorted(f for f in os.listdir(os.path.join(ROOT, name))
                       if not f.startswith(".") and f.lower().endswith((".jpg", ".jpeg", ".png"))
                       and os.path.join(ROOT, name, f) in cf)
        qfile = os.path.join(ROOT, name, files[0])
        qface = cf[qfile]
        r = {"name": name, "truth": len(truth), "actions": 0, "ok": True}
        pg.goto(BASE + "/")
        t = time.time()
        pg.set_input_files("input[name=image]", qfile)
        r["actions"] += 1
        pg.wait_for_url("**/query/**", timeout=60000)
        pg.wait_for_load_state("networkidle")
        r["t_upload"] = time.time() - t
        # the row containing the query photo's own central face (similarity ~1)
        section = pg.locator(f"section.queryface:has(label.face[data-id='{qface}'])")
        if section.count() == 0:
            r["ok"] = False
            results.append(r)
            print(f"  {name}: FAILED — query face not offered")
            continue
        sec = section.first
        with pg.expect_response(lambda resp: "/api/assign" in resp.url, timeout=300000) as resp_info:
            if KEYBOARD:
                sec.locator(".assign-name").focus()
                pg.keyboard.type(name.replace("_", " "))
                pg.keyboard.press("Tab")   # -> Assign button
                t = time.time()
                pg.keyboard.press("Enter")
            else:
                sec.locator(".assign-name").fill(name.replace("_", " "))
                t = time.time()
                sec.locator("[data-action=assign]").click()
        r["actions"] += 2
        why = assign_failed(resp_info.value)
        if why:
            # the page stays put so the message can be read: no navigation
            r["ok"] = False
            results.append(r)
            print(f"  {name}: FAILED — assign: {why}")
            continue
        pg.wait_for_url("**/person/**", timeout=120000)
        pg.wait_for_load_state("networkidle")
        r["t_assign"] = time.time() - t
        r["p1"], r["r1"], r["n1"] = score(con, name.replace("_", " "), truth)
        # step 3: confirm pre-ticked suggestions
        btn = pg.locator("[data-action=confirm]")
        # Confirm only sends a request when some suggestion is ticked (the
        # page pre-ticks those with similarity >= 0.5); otherwise there is
        # nothing to confirm and no response to wait for.
        ticked = pg.locator("section.picker-scope:has([data-action=confirm]) .pick:checked").count() if btn.count() else 0
        if btn.count() and not ticked:
            r["confirm_skipped"] = True
        if ticked:
            t = time.time()
            # Score only after the server has answered: /api/assign returns
            # once the labels are saved and auto-matching has finished.
            with pg.expect_response(lambda resp: "/api/assign" in resp.url, timeout=300000) as resp_info:
                if KEYBOARD:
                    btn.first.focus()
                    pg.keyboard.press("Enter")
                else:
                    btn.first.click()
            why = assign_failed(resp_info.value)
            if why:
                r["ok"] = False
                print(f"  {name}: FAILED — confirm: {why}")
            r["actions"] += 1
            r["t_confirm"] = time.time() - t
        r["p2"], r["r2"], r["n2"] = score(con, name.replace("_", " "), truth)
        results.append(r)
        print(f"  {name:<28} photos {len(truth):>3} | after assign: P {r['p1']:.3f} R {r['r1']:.3f} | "
              f"after confirm: P {r['p2']:.3f} R {r['r2']:.3f} | actions {r['actions']} | "
              f"upload {r['t_upload']:.1f}s assign {r['t_assign']:.1f}s")
    b.close()

ok = [r for r in results if r["ok"]]
print(f"\nTask completion: {len(ok)}/{len(results)}")
failed = [r["name"] for r in results if not r["ok"]]
if ok:
    m = lambda k: statistics.mean(r[k] for r in ok if k in r)
    print(f"Mean actions: {m('actions'):.1f}")
    print(f"After step 2 (assign): precision {m('p1'):.3f}, recall {m('r1'):.3f}")
    print(f"After step 3 (confirm): precision {m('p2'):.3f}, recall {m('r2'):.3f}")
    print(f"Median step time: upload+analyse {statistics.median(r['t_upload'] for r in ok):.2f}s, "
          f"assign+auto-match {statistics.median(r['t_assign'] for r in ok):.2f}s")
if failed:
    # a usability test that could not complete its task must fail the run
    sys.exit(f"task not completed for: {', '.join(failed)}")
