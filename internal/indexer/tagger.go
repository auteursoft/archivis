package indexer

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/auteursoft/archivis/internal/ml"
	"github.com/auteursoft/archivis/internal/store"
)

// DefaultCategories are zero-shot CLIP classes geared to a photojournalist's
// archive. Each line is "tag: prompt | prompt | ...". Users can override them
// with a categories.txt file in the data directory.
const DefaultCategories = `
portrait: a portrait photo of a person | a close-up headshot of a person
group: a photo of a small group of people posing together
crowd: a photo of a large crowd of people
protest: a photo of a protest or demonstration with signs | protesters marching in the street
speech: a person giving a speech at a podium | a press conference with microphones
meeting: people sitting around a table in a meeting | an office meeting
sports: a photo of an athletic sports game | athletes competing in a sport
concert: a concert or live music performance on stage | a theater performance
ceremony: a wedding ceremony | a graduation ceremony | a formal ceremony
children: a photo of children playing | a photo of a child
classroom: students in a classroom | a lecture hall
police: police officers on duty | emergency responders at a scene
fire: a fire with flames and smoke | firefighters fighting a fire
disaster: a flooded street | destruction after a storm or disaster
military: soldiers in uniform | military vehicles
politics: a political rally with campaign signs | a politician shaking hands
street: a street photo of people in a city
cityscape: a city skyline | a cityscape with buildings
architecture: a photo of a building's architecture | a building interior
landscape: a landscape photo of mountains or fields | a scenic natural landscape
water: a beach or the ocean | a lake or river
snow: a snowy winter scene
night: a photo taken at night with city lights | a dark night scene
sunset: a sunset or sunrise sky
wildlife: a wild animal in nature | a bird
pet: a photo of a dog | a photo of a cat
food: a photo of food on a plate | a meal
vehicle: a photo of a car | a train or airplane
aerial: an aerial photo taken from a drone or airplane
document: a scanned document or page of text | a screenshot of a computer screen | a receipt or form
`

// Tagger assigns category tags from CLIP image embeddings.
type Tagger struct {
	names   []string
	owner   []int       // prompt index -> category index
	prompts [][]float32 // text embeddings
	// MinProb is the minimum softmax probability for a tag to be kept.
	MinProb float64
	MaxTags int
}

// ParseCategories reads the "tag: prompt | prompt" format.
func ParseCategories(text string) ([][2]string, error) {
	var out [][2]string
	var bad []string
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rest, ok := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		var prompts []string
		for _, p := range strings.Split(rest, "|") {
			if p = strings.TrimSpace(p); p != "" {
				prompts = append(prompts, p)
			}
		}
		if !ok || name == "" || len(prompts) == 0 {
			// A typo must not silently drop a category: retag would then
			// strip that category from every photo.
			bad = append(bad, fmt.Sprintf("line %d: %q", n, line))
			continue
		}
		for _, p := range prompts {
			out = append(out, [2]string{name, p})
		}
	}
	// A read error (e.g. a line over the scanner limit) must not pass the
	// lines before it off as the whole file.
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("categories: %w", err)
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("categories: each line must be `name: description | description …`; fix %s", strings.Join(bad, ", "))
	}
	if len(out) == 0 {
		return nil, errors.New("categories: no categories defined (delete the file to use the built-in ones)")
	}
	return out, nil
}

// LoadCategories returns the user's categories file, or the defaults if
// there is none. Any other read error is returned: silently falling back to
// the defaults would let `retag` replace the user's custom tags.
func LoadCategories(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return DefaultCategories, nil
	}
	if err != nil {
		return "", fmt.Errorf("reading categories: %w", err)
	}
	return string(b), nil
}

// NewTagger embeds every prompt with the CLIP text encoder.
func NewTagger(c *ml.CLIP, categories string) (*Tagger, error) {
	t := &Tagger{MinProb: 0.2, MaxTags: 3}
	idx := map[string]int{}
	cats, err := ParseCategories(categories)
	if err != nil {
		return nil, err
	}
	for _, cp := range cats {
		ci, ok := idx[cp[0]]
		if !ok {
			ci = len(t.names)
			idx[cp[0]] = ci
			t.names = append(t.names, cp[0])
		}
		e, err := c.EmbedText(cp[1])
		if err != nil {
			return nil, err
		}
		t.owner = append(t.owner, ci)
		t.prompts = append(t.prompts, e)
	}
	return t, nil
}

// Tags returns the categories an image embedding belongs to.
func (t *Tagger) Tags(emb []float32) []store.Tag {
	if t == nil || len(t.prompts) == 0 {
		return nil
	}
	// CLIP's learned logit scale is 100; softmax over all prompts, then sum
	// probability mass per category.
	logits := make([]float64, len(t.prompts))
	mx := math.Inf(-1)
	for i, p := range t.prompts {
		logits[i] = 100 * float64(ml.Dot(p, emb))
		mx = math.Max(mx, logits[i])
	}
	var z float64
	for i := range logits {
		logits[i] = math.Exp(logits[i] - mx)
		z += logits[i]
	}
	probs := make([]float64, len(t.names))
	for i, l := range logits {
		probs[t.owner[i]] += l / z
	}
	var out []store.Tag
	for i, p := range probs {
		if p >= t.MinProb {
			out = append(out, store.Tag{Tag: t.names[i], Score: math.Round(p*1000) / 1000})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > t.MaxTags {
		out = out[:t.MaxTags]
	}
	return out
}
