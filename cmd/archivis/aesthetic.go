package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"archivis/internal/indexer"
	"archivis/internal/ml"
	"archivis/internal/store"
)

func runAesthetic(g *globals, args []string) error {
	fs := newFlagSet("aesthetic", `Aesthetic models: every model a catalogue has used is kept, with its
score for every photo; the aesthetic score shown is a weighted blend of the
active models.
  aesthetic models                list models, blend weights and feedback
  aesthetic train                 fit a model to the ratings and verdicts given
                                  on photo pages; it joins the blend only if it
                                  predicts held-out judgements better
  aesthetic use ID[=WEIGHT]...    set the blend (IDs may be abbreviated)
  aesthetic import FILE           register a model JSON {"name","w","b"}
  aesthetic export ID             print a model's JSON`)
	rater := fs.String("rater", "", "train: use only this rater's judgements (default: everyone's)")
	name := fs.String("name", "", "train: name of the new model (default: the rater, or \"user\")")
	minN := fs.Int("min", 20, "train: minimum number of judgements")
	dry := fs.Bool("dry-run", false, "train: evaluate and report, but do not save the model")
	force := fs.Bool("force", false, "train: add the model to the blend even if it is not better")
	g.register(fs)
	pos := parseInterleaved(fs, args)
	if len(pos) == 0 {
		pos = []string{"models"}
	}
	// The database alone: these commands need no vector index.
	if err := os.MkdirAll(g.data, 0o755); err != nil {
		return err
	}
	if pos[0] == "use" || (pos[0] == "train" && !*dry) {
		// Changing the blend under a running index would leave it scoring
		// new photos with the previous models until its next run.
		unlock, err := indexer.LockIndex(g.data)
		if errors.Is(err, indexer.ErrIndexRunning) {
			return fmt.Errorf("%w: run `aesthetic %s` when it has finished (--dry-run works meanwhile)", err, pos[0])
		}
		if err != nil {
			return err
		}
		defer unlock()
	}
	st, err := store.Open(indexer.Layout{Dir: g.data}.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := indexer.SyncAesthetics(st, ml.DefaultAesthetic()); err != nil {
		return fmt.Errorf("aesthetic models: %w", err)
	}
	switch pos[0] {
	case "models":
		if len(pos) != 1 {
			return fmt.Errorf("usage: aesthetic models")
		}
		return listAesthetic(st)
	case "train":
		if len(pos) != 1 {
			return fmt.Errorf("usage: aesthetic train [--rater NAME] [--name NAME] [--dry-run] [--force]")
		}
		fb, embs, err := st.LatestFeedback(*rater)
		if err != nil {
			return err
		}
		active, err := indexer.ActiveAesthetics(st)
		if err != nil {
			return err
		}
		res, err := indexer.TrainAesthetic(indexer.FeedbackSamples(fb, embs), active,
			indexer.TrainOptions{Name: cmp.Or(*name, *rater, "user"), MinSamples: *minN})
		if err != nil {
			return err
		}
		fmt.Printf("trained %s on %d judgements (%d ratings, %d verdicts), λ=%g\n",
			res.Model.ID, res.Samples, res.Ratings, res.Samples-res.Ratings, res.Lambda)
		fmt.Printf("on judgements held out from fitting:\n")
		fmt.Printf("  %-28s error %.2f points%s\n", "current blend", res.PriorRMSE, rho(res.PriorRho))
		fmt.Printf("  %-28s error %.2f points%s\n", "new model", res.RMSE, rho(res.Rho))
		if res.Mix > 0 && res.Mix < 1 {
			fmt.Printf("  %-28s error %.2f points%s\n", fmt.Sprintf("blend with %.0f%% new model", res.Mix*100), res.BlendRMSE, rho(res.BlendRho))
		}
		switch {
		case *dry:
			fmt.Println("dry run: nothing saved")
			return nil
		case res.Better || *force:
			if err := indexer.SaveTrained(st, res, true); err != nil {
				return err
			}
			fmt.Printf("saved and added to the blend; aesthetic scores updated (%s)\n", weightsText(res.Weights))
		default:
			if err := indexer.SaveTrained(st, res, false); err != nil {
				return err
			}
			fmt.Println("saved but not added to the blend: it is not clearly better yet (more judgements help;",
				"--force adds it anyway, or `aesthetic use` later)")
		}
		return nil
	case "use":
		if len(pos) < 2 {
			return fmt.Errorf("usage: aesthetic use ID[=WEIGHT]...")
		}
		weights := map[string]float64{}
		for _, a := range pos[1:] {
			idText, wText, hasW := strings.Cut(a, "=")
			id, err := findAesthetic(st, idText)
			if err != nil {
				return err
			}
			w := 1.0
			if hasW {
				if w, err = strconv.ParseFloat(wText, 64); err != nil || !(w > 0) || math.IsInf(w, 0) {
					return fmt.Errorf("%s: weight must be a positive number", a)
				}
			}
			weights[id] = w
		}
		if err := indexer.Activate(st, weights); err != nil {
			return err
		}
		fmt.Printf("blend set; aesthetic scores updated (%s)\n", weightsText(weights))
		return nil
	case "import":
		if len(pos) != 2 {
			return fmt.Errorf("usage: aesthetic import FILE")
		}
		b, err := os.ReadFile(pos[1])
		if err != nil {
			return err
		}
		a, err := ml.ParseAesthetic(b)
		if err != nil {
			return err
		}
		added, err := st.AddAestheticModel(store.AestheticModel{ID: a.ID, Name: a.Name, Kind: "imported", Model: a.JSON()})
		if err != nil {
			return err
		}
		if !added {
			fmt.Printf("%s is already registered\n", a.ID)
			return nil
		}
		fmt.Printf("registered %s; add it to the blend with `archivis aesthetic use`\n", a.ID)
		return nil
	case "export":
		if len(pos) != 2 {
			return fmt.Errorf("usage: aesthetic export ID")
		}
		id, err := findAesthetic(st, pos[1])
		if err != nil {
			return err
		}
		ms, err := st.AestheticModels()
		if err != nil {
			return err
		}
		for _, m := range ms {
			if m.ID == id {
				if len(m.Model) == 0 {
					return fmt.Errorf("%s holds earlier scores only; its weights are unknown", id)
				}
				fmt.Println(string(m.Model))
			}
		}
		return nil
	}
	return fmt.Errorf("unknown aesthetic command %q (models, train, use, import, export)", pos[0])
}

func listAesthetic(st *store.Store) error {
	ms, err := st.AestheticModels()
	if err != nil {
		return err
	}
	fmt.Printf("%-28s %-8s %-7s %9s  %-10s  %s\n", "MODEL", "KIND", "WEIGHT", "PHOTOS", "ADDED", "NOTES")
	for _, m := range ms {
		w := "-"
		if m.Active {
			w = strconv.FormatFloat(m.Weight, 'g', 3, 64)
		}
		note := metricsNote(m.Metrics)
		if len(m.Model) == 0 {
			note = "earlier scores, kept for the record"
		}
		fmt.Printf("%-28s %-8s %-7s %9d  %-10s  %s\n", m.ID, m.Kind, w, m.Scored, time.Unix(m.CreatedAt, 0).Format("2006-01-02"), note)
	}
	sum, err := st.FeedbackSummary()
	if err != nil {
		return err
	}
	fmt.Printf("\n%d photos judged", sum.Photos)
	for _, r := range sum.Raters {
		fmt.Printf("; %s %d", r.Rater, r.Photos)
	}
	if sum.NoEmbedding > 0 {
		fmt.Printf(" (%d without a CLIP embedding cannot be used for training)", sum.NoEmbedding)
	}
	fmt.Println()
	return nil
}

// findAesthetic resolves a model ID or a unique prefix of one.
func findAesthetic(st *store.Store, prefix string) (string, error) {
	ms, err := st.AestheticModels()
	if err != nil {
		return "", err
	}
	var hits []string
	for _, m := range ms {
		if m.ID == prefix {
			return m.ID, nil
		}
		if strings.HasPrefix(m.ID, prefix) {
			hits = append(hits, m.ID)
		}
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no aesthetic model %q (see `archivis aesthetic models`)", prefix)
	case 1:
		return hits[0], nil
	}
	return "", fmt.Errorf("%q matches several models: %s", prefix, strings.Join(hits, ", "))
}

func weightsText(w map[string]float64) string {
	var parts []string
	for id, v := range w {
		parts = append(parts, fmt.Sprintf("%s ×%.2g", id, v))
	}
	return strings.Join(parts, ", ")
}

func rho(v float64) string {
	if math.IsNaN(v) {
		return ""
	}
	return fmt.Sprintf(", rank agreement with ratings %.2f", v)
}

// metricsNote summarises a trained model's evaluation.
func metricsNote(js string) string {
	var m struct {
		Samples   int      `json:"samples"`
		PriorRMSE *float64 `json:"prior_rmse"`
		RMSE      *float64 `json:"rmse"`
	}
	if js == "" || json.Unmarshal([]byte(js), &m) != nil || m.PriorRMSE == nil || m.RMSE == nil {
		return ""
	}
	return fmt.Sprintf("trained on %d judgements; held-out error %.2f -> %.2f", m.Samples, *m.PriorRMSE, *m.RMSE)
}
