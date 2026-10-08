package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/auteursoft/archivis/internal/indexer"
	"github.com/auteursoft/archivis/internal/ml"
)

// personalCmd measures how well training on one person's judgements
// (archivis aesthetic train) predicts that person's ratings of photos it has
// not seen, using the individual votes in EVA (the dataset behind the
// built-in model, ~30 raters per photo, some raters with thousands of votes).
//
// For each rater, EVA's photos are split in two by photo. A stand-in for the
// built-in model is fit to the mean rating of the *other* raters on half A,
// so it has never seen the test photos or this rater. The rater's votes on
// half B are their feedback: n of them train a model exactly as the app
// does, the rest are the test. Feedback is either the ratings themselves or
// only verdicts on the score shown (too high / about right / too low, when
// the rating differs from it by more than one point).
//
//	validate personal -eva EVA_DIR -clip eva_clip.csv -out personal.csv
func personalCmd(args []string) error {
	fs := flag.NewFlagSet("personal", flag.ExitOnError)
	evaDir := fs.String("eva", "", "EVA dataset directory")
	clipCSV := fs.String("clip", "", "CLIP embeddings of EVA_together (validate clip output)")
	out := fs.String("out", "personal.csv", "per-trial CSV")
	nRaters := fs.Int("raters", 25, "raters to simulate (those with the most votes)")
	sizes := fs.String("n", "20,50,100,200", "feedback sizes")
	reps := fs.Int("reps", 3, "random repetitions per rater and size")
	minTest := fs.Int("min-test", 200, "minimum test photos per trial")
	fs.Parse(args)

	emb := map[string][]float32{}
	f, err := os.Open(*clipCSV)
	if err != nil {
		return err
	}
	rows, err := csv.NewReader(f).ReadAll()
	f.Close()
	if err != nil {
		return err
	}
	for _, r := range rows {
		v := make([]float32, len(r)-1)
		for i, s := range r[1:] {
			x, _ := strconv.ParseFloat(s, 32)
			v[i] = float32(x)
		}
		emb[strings.TrimSuffix(r[0], ".jpg")] = ml.Normalize(v)
	}
	vf, err := os.Open(*evaDir + "/data/votes_filtered.csv")
	if err != nil {
		return err
	}
	cr := csv.NewReader(vf)
	cr.Comma = '='
	cr.FieldsPerRecord = -1
	votes, err := cr.ReadAll()
	vf.Close()
	if err != nil {
		return err
	}
	byPhoto := map[string]map[string]float64{} // photo -> rater -> score
	byRater := map[string]map[string]float64{}
	for _, v := range votes[1:] {
		if _, ok := emb[v[0]]; !ok {
			continue
		}
		s, err := strconv.ParseFloat(v[2], 64)
		if err != nil {
			continue
		}
		if byPhoto[v[0]] == nil {
			byPhoto[v[0]] = map[string]float64{}
		}
		if byRater[v[1]] == nil {
			byRater[v[1]] = map[string]float64{}
		}
		byPhoto[v[0]][v[1]], byRater[v[1]][v[0]] = s, s
	}
	var raters []string
	for r := range byRater {
		raters = append(raters, r)
	}
	sort.Slice(raters, func(i, j int) bool {
		if len(byRater[raters[i]]) != len(byRater[raters[j]]) {
			return len(byRater[raters[i]]) > len(byRater[raters[j]])
		}
		return raters[i] < raters[j]
	})
	raters = raters[:min(*nRaters, len(raters))]
	var photos []string
	for p := range byPhoto {
		photos = append(photos, p)
	}
	sort.Strings(photos)
	var ns []int
	for _, s := range strings.Split(*sizes, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return err
		}
		ns = append(ns, n)
	}

	o, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer o.Close()
	w := csv.NewWriter(o)
	defer w.Flush()
	w.Write([]string{"rater", "votes", "n", "rep", "feedback", "test", "lambda", "mix", "activated",
		"prior_rmse", "new_rmse", "deployed_rmse", "prior_rho", "new_rho", "deployed_rho"})
	type key struct {
		n    int
		mode string
	}
	agg := map[key][][4]float64{} // prior rho, deployed rho, prior rmse, deployed rmse
	activated := map[key]int{}
	for ri, rater := range raters {
		rng := rand.New(rand.NewSource(int64(ri) + 1))
		inA := map[string]bool{}
		for _, p := range photos {
			inA[p] = rng.Intn(2) == 0
		}
		// stand-in built-in model: other raters' mean on half A
		var x [][]float32
		var y, c []float64
		for _, p := range photos {
			if !inA[p] {
				continue
			}
			var s float64
			k := 0
			for r, v := range byPhoto[p] {
				if r != rater {
					s += v
					k++
				}
			}
			if k >= 10 {
				x, y, c = append(x, emb[p]), append(y, s/float64(k)), append(c, 1)
			}
		}
		mean := 0.0
		for _, v := range y {
			mean += v
		}
		mean /= float64(len(y))
		pw, pb, err := ml.FitRidgeToward(x, y, c, make([]float32, ml.EmbedDim), float32(mean), 0.66)
		if err != nil {
			return err
		}
		prior, _ := ml.NewAesthetic("prior", pw, pb)
		active := []indexer.AestheticModel{{Aesthetic: prior, Weight: 1}}
		var pool []string
		for p := range byRater[rater] {
			if !inA[p] {
				pool = append(pool, p)
			}
		}
		sort.Strings(pool)
		for _, n := range ns {
			if len(pool)-n < *minTest {
				continue
			}
			for rep := 0; rep < *reps; rep++ {
				perm := rng.Perm(len(pool))
				train, test := perm[:n], perm[n:]
				for _, mode := range []string{"ratings", "verdicts"} {
					var samples []indexer.Sample
					for _, i := range train {
						p := pool[i]
						r := byRater[rater][p]
						s := indexer.Sample{X: emb[p], Y: r, C: 1, Rating: true}
						if mode == "verdicts" {
							shown := float64(prior.Score(emb[p]))
							s.Rating, s.C, s.Y = false, indexer.VerdictWeight, shown
							if r < shown-1 {
								s.Y = shown - indexer.VerdictStep
							} else if r > shown+1 {
								s.Y = shown + indexer.VerdictStep
							}
						}
						samples = append(samples, s)
					}
					res, err := indexer.TrainAesthetic(samples, active, indexer.TrainOptions{MinSamples: 1, Seed: int64(rep)})
					if err != nil {
						return err
					}
					var pp, np, dp, truth []float64
					for _, i := range test {
						p := pool[i]
						a, b := float64(prior.Score(emb[p])), float64(res.Model.Score(emb[p]))
						d := a
						if res.Better {
							d = res.Mix*b + (1-res.Mix)*a
						}
						pp, np, dp, truth = append(pp, a), append(np, b), append(dp, d), append(truth, byRater[rater][p])
					}
					row := []float64{rmse(pp, truth), rmse(np, truth), rmse(dp, truth),
						indexer.Spearman(pp, truth), indexer.Spearman(np, truth), indexer.Spearman(dp, truth)}
					rec := []string{rater, strconv.Itoa(len(byRater[rater])), strconv.Itoa(n), strconv.Itoa(rep), mode,
						strconv.Itoa(len(test)), fmt.Sprint(res.Lambda), fmt.Sprint(res.Mix), fmt.Sprint(res.Better)}
					for _, v := range row {
						rec = append(rec, fmt.Sprintf("%.4f", v))
					}
					w.Write(rec)
					k := key{n, mode}
					agg[k] = append(agg[k], [4]float64{row[3], row[5], row[0], row[2]})
					if res.Better {
						activated[k]++
					}
				}
			}
		}
		fmt.Fprintf(os.Stderr, "rater %d/%d (%s, %d votes) done\n", ri+1, len(raters), rater, len(byRater[rater]))
	}
	fmt.Printf("%-5s %-9s %6s %10s %22s %22s %12s\n", "n", "feedback", "trials", "activated", "rank agreement ρ", "error (points)", "ρ improved")
	for _, mode := range []string{"ratings", "verdicts"} {
		for _, n := range ns {
			k := key{n, mode}
			v := agg[k]
			if len(v) == 0 {
				continue
			}
			var pr, dr, pe, de []float64
			better := 0
			for _, t := range v {
				pr, dr, pe, de = append(pr, t[0]), append(dr, t[1]), append(pe, t[2]), append(de, t[3])
				if t[1] > t[0] {
					better++
				}
			}
			fmt.Printf("%-5d %-9s %6d %9.0f%% %10.3f -> %.3f %10.2f -> %.2f %11.0f%%\n", n, mode, len(v),
				100*float64(activated[k])/float64(len(v)), median(pr), median(dr), median(pe), median(de), 100*float64(better)/float64(len(v)))
		}
	}
	return nil
}

func rmse(p, y []float64) float64 {
	var s float64
	for i := range p {
		s += (p[i] - y[i]) * (p[i] - y[i])
	}
	return math.Sqrt(s / float64(len(p)))
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}
