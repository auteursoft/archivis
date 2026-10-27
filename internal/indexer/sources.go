package indexer

import (
	"context"
	"math"
	"strings"

	"github.com/auteursoft/archivis/internal/ml"
	"github.com/auteursoft/archivis/internal/store"
	"github.com/auteursoft/archivis/internal/vindex"
)

// Sources tells memes and screenshots from photos by their CLIP image
// embedding: the share of probability, over the prompts below, that goes to
// the meme and screenshot ones. Only photos whose metadata names no camera
// are asked (what a camera took is a camera photo, whatever it shows), so a
// real photo is called a meme only when it was also stripped of its
// metadata (sent through a messaging app, downloaded) and looks like one.
//
// On a test set of 126 made-up memes, posts, quote cards, news and sports
// graphics and phone screenshots, and 520 real photos (BIQ2021 phone
// photos, EVA, pets, ImageNet product shots), the threshold of 0.6 caught
// 117 of the memes and called 12 photos memes, mostly lettered products and
// graphic close-ups.
type Sources struct {
	prompts [][]float32
}

// memePrompts come first in sourcePrompts; the rest describe photos.
const memePrompts = 11

var sourcePrompts = strings.Split(strings.TrimSpace(`
a meme with bold caption text over a picture
a screenshot of a social media post
a screenshot of a phone screen
a screenshot of a text message conversation
a news graphic with text and numbers
a graphic with large text
a poster or flyer with text
a quote on a colored background
a screenshot of a website or app
a screenshot of a post with a caption above a photo
a sports graphic with players and large text
a photo
a photo of people
a photo of a person
a photo of a landscape
a photo of an animal
a photo of an object
a photo of a building
a snapshot taken with a phone camera
a photo of food
a photo of a street
a photo of a sign with text
a photo of a document or page of text
a product photo of a device
a close-up photo of something with writing on it
a photo of toys
a photo of a machine`), "\n")

// MemeThreshold is the share of probability on the meme and screenshot
// prompts from which an image counts as one.
const MemeThreshold = 0.6

// NewSources embeds the prompts with the CLIP text encoder.
func NewSources(c *ml.CLIP) (*Sources, error) {
	s := &Sources{}
	for _, p := range sourcePrompts {
		e, err := c.EmbedText(p)
		if err != nil {
			return nil, err
		}
		s.prompts = append(s.prompts, e)
	}
	return s, nil
}

// MemeProb is how likely the image is a meme or screenshot (0..1).
func (s *Sources) MemeProb(emb []float32) float64 {
	// softmax at CLIP's logit scale (100), as the tagger
	logits := make([]float64, len(s.prompts))
	mx := math.Inf(-1)
	for i, p := range s.prompts {
		logits[i] = 100 * float64(ml.Dot(p, emb))
		mx = math.Max(mx, logits[i])
	}
	var z, memes float64
	for i, l := range logits {
		e := math.Exp(l - mx)
		z += e
		if i < memePrompts {
			memes += e
		}
	}
	return memes / z
}

// Of works out a photo's source: a camera photo if its metadata names a
// camera; otherwise, from its embedding, a meme or screenshot or a saved
// image. Without an embedding of the classifier's size (or a classifier)
// it stays unknown, to be worked out once there is one.
func (s *Sources) Of(camera bool, emb []float32) int {
	switch {
	case camera:
		return store.SourceCamera
	case s == nil || len(s.prompts) == 0 || len(emb) != len(s.prompts[0]):
		// none yet, or of another size (a damaged row, another model): left
		// to work out later, the photo shown meanwhile
		return store.SourceUnknown
	case s.MemeProb(emb) >= MemeThreshold:
		return store.SourceMeme
	}
	return store.SourceSaved
}

// ClassifySources works out the source of every photo not yet worked out
// (or, with all, of every photo, as after the rules change), from what is
// stored: nothing is read from the photos' files. Without a classifier
// (no CLIP text model) only camera photos are worked out. It returns how
// many photos it worked out.
func ClassifySources(ctx context.Context, st *store.Store, s *Sources, all bool) (int, error) {
	n := 0
	for after := int64(0); ; {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		batch, err := st.SourceInputs(after, all, 2000)
		if err != nil || len(batch) == 0 {
			return n, err
		}
		var got []store.SourceSet
		for _, in := range batch {
			var emb []float32
			if in.CLIP != nil {
				emb = vindex.DecodeF16(in.CLIP)
			}
			if v := s.Of(in.Camera, emb); v != store.SourceUnknown {
				got = append(got, store.SourceSet{ID: in.ID, Seq: in.Seq, Source: v})
			}
			after = in.ID
		}
		if err := st.SetSources(got); err != nil {
			return n, err
		}
		n += len(got)
	}
}
