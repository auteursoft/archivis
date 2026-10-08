package ml

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"html"
	"io"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
	"golang.org/x/text/width"
)

// CLIP byte-pair-encoding vocabulary (MIT licensed, from openai/CLIP).
//
//go:embed assets/bpe_simple_vocab_16e6.txt.gz
var bpeVocabGz []byte

const (
	tokSOT       = 49406
	tokEOT       = 49407
	contextWidth = 77
)

// Tokenizer is a Go port of CLIP's SimpleTokenizer.
type Tokenizer struct {
	byteEnc [256]string
	encoder map[string]int32
	ranks   map[[2]string]int
	pat     *regexp.Regexp
	mu      sync.Mutex
	cache   map[string][]string
}

var (
	tokOnce sync.Once
	tokInst *Tokenizer
	tokErr  error
)

// DefaultTokenizer returns the shared tokenizer built from the embedded vocab.
func DefaultTokenizer() (*Tokenizer, error) {
	tokOnce.Do(func() { tokInst, tokErr = newTokenizer(bpeVocabGz) })
	return tokInst, tokErr
}

// bytesToUnicode returns CLIP's reversible byte->rune table, in the order the
// reference implementation enumerates it (printable bytes first). The order
// matters: it defines the first 512 vocabulary ids.
func bytesToUnicode() (order []byte, table [256]rune) {
	var bs []int
	for b := '!'; b <= '~'; b++ {
		bs = append(bs, int(b))
	}
	for b := '¡'; b <= '¬'; b++ {
		bs = append(bs, int(b))
	}
	for b := '®'; b <= 'ÿ'; b++ {
		bs = append(bs, int(b))
	}
	in := map[int]bool{}
	for _, b := range bs {
		in[b] = true
		table[b] = rune(b)
	}
	n := 0
	for b := 0; b < 256; b++ {
		if !in[b] {
			bs = append(bs, b)
			table[b] = rune(256 + n)
			n++
		}
	}
	for _, b := range bs {
		order = append(order, byte(b))
	}
	return order, table
}

func newTokenizer(gz []byte) (*Tokenizer, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(raw), "\n")
	lines = lines[1 : 49152-256-2+1]
	t := &Tokenizer{
		encoder: make(map[string]int32, 49408),
		ranks:   make(map[[2]string]int, len(lines)),
		cache:   map[string][]string{},
		pat:     regexp.MustCompile(`<\|startoftext\|>|<\|endoftext\|>|'s|'t|'re|'ve|'m|'ll|'d|\p{L}+|\p{N}|[^\s\p{L}\p{N}]+`),
	}
	order, b2u := bytesToUnicode()
	for i, r := range b2u {
		t.byteEnc[i] = string(r)
	}
	vocab := make([]string, 0, 49408)
	for _, b := range order {
		vocab = append(vocab, string(b2u[b]))
	}
	for i := 0; i < 256; i++ {
		vocab = append(vocab, vocab[i]+"</w>")
	}
	for i, l := range lines {
		p := strings.Fields(l)
		if len(p) != 2 {
			continue
		}
		t.ranks[[2]string{p[0], p[1]}] = i
		vocab = append(vocab, p[0]+p[1])
	}
	vocab = append(vocab, "<|startoftext|>", "<|endoftext|>")
	for i, v := range vocab {
		t.encoder[v] = int32(i)
	}
	return t, nil
}

func (t *Tokenizer) bpe(token string) []string {
	t.mu.Lock()
	if c, ok := t.cache[token]; ok {
		t.mu.Unlock()
		return c
	}
	t.mu.Unlock()
	// split into "characters" (here: the byte-encoded runes)
	var word []string
	for _, r := range token {
		word = append(word, string(r))
	}
	if len(word) == 0 {
		return nil
	}
	word[len(word)-1] += "</w>"
	for len(word) > 1 {
		best, bestRank := -1, int(^uint(0)>>1)
		for i := 0; i+1 < len(word); i++ {
			if r, ok := t.ranks[[2]string{word[i], word[i+1]}]; ok && r < bestRank {
				best, bestRank = i, r
			}
		}
		if best < 0 {
			break
		}
		first, second := word[best], word[best+1]
		var nw []string
		for i := 0; i < len(word); {
			if i < len(word)-1 && word[i] == first && word[i+1] == second {
				nw = append(nw, first+second)
				i += 2
			} else {
				nw = append(nw, word[i])
				i++
			}
		}
		word = nw
	}
	if len(token) <= 64 { // cache ordinary words only
		t.mu.Lock()
		if len(t.cache) < 100000 {
			t.cache[token] = word
		}
		t.mu.Unlock()
	}
	return word
}

func cleanText(s string) string {
	s = fixText(s)
	s = html.UnescapeString(html.UnescapeString(s))
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// Encode returns BPE token ids for text (without start/end tokens).
func (t *Tokenizer) Encode(text string) []int32 {
	return t.encode(text, -1)
}

// maxWordRunes caps a single word before byte-pair merging, which is
// roughly quadratic in word length; no real query word comes close.
const maxWordRunes = 200

// encode tokenizes text, stopping once limit ids are produced (-1: no
// limit). The reference implementation truncates after encoding, so
// stopping early gives the same first ids while bounding the work a long
// query can cause.
func (t *Tokenizer) encode(text string, limit int) []int32 {
	var ids []int32
	if len(text) > 1<<16 {
		text = text[:1<<16] // way past what fits in a CLIP context
	}
	for _, tok := range t.pat.FindAllString(cleanText(text), -1) {
		if limit >= 0 && len(ids) >= limit {
			break
		}
		if r := []rune(tok); len(r) > maxWordRunes {
			tok = string(r[:maxWordRunes])
		}
		var sb strings.Builder
		for _, c := range []byte(tok) {
			sb.WriteString(t.byteEnc[c])
		}
		for _, piece := range t.bpe(sb.String()) {
			if id, ok := t.encoder[piece]; ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// Tokenize returns a context-length row with start/end tokens and the
// attention mask, truncating long text as CLIP does.
func (t *Tokenizer) Tokenize(text string) (ids, mask []int32) {
	body := t.encode(text, contextWidth-2)
	if len(body) > contextWidth-2 {
		body = body[:contextWidth-2]
	}
	ids = make([]int32, contextWidth)
	mask = make([]int32, contextWidth)
	ids[0] = tokSOT
	copy(ids[1:], body)
	ids[len(body)+1] = tokEOT
	for i := 0; i < len(body)+2; i++ {
		mask[i] = 1
	}
	return ids, mask
}

// fixText applies the parts of ftfy.fix_text (which the reference CLIP
// tokenizer runs first) that change real queries: repair of UTF-8 text that
// was decoded as Windows-1252 ("cafÃ©"), straight quotes, expanded Latin
// ligatures, folded full/half-width forms and NFC normalisation, so that
// equivalent spellings produce the same tokens. Verified against ftfy with
// tools/validate/clip_tokenizer_ref.py (testdata/tok_ref.json).
func fixText(s string) string {
	s = fixMojibake(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == 0x02bc || (r >= 0x2018 && r <= 0x201b):
			b.WriteByte('\'')
		case r >= 0x201c && r <= 0x201f:
			b.WriteByte('"')
		default:
			if l, ok := ligatures[r]; ok {
				b.WriteString(l)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return norm.NFC.String(width.Fold.String(b.String()))
}

// ligatures is ftfy's chardata.LIGATURES.
var ligatures = map[rune]string{
	0x0132: "IJ", 0x0133: "ij", 0x0149: "ʼn", 0x01f1: "DZ", 0x01f2: "Dz", 0x01f3: "dz",
	0x01c4: "DŽ", 0x01c5: "Dž", 0x01c6: "dž", 0x01c7: "LJ", 0x01c8: "Lj", 0x01c9: "lj",
	0x01ca: "NJ", 0x01cb: "Nj", 0x01cc: "nj", 0xfb00: "ff", 0xfb01: "fi", 0xfb02: "fl",
	0xfb03: "ffi", 0xfb04: "ffl", 0xfb05: "ſt", 0xfb06: "st",
}

// cp1252 maps the Windows-1252 characters in 0x80-0x9F back to their bytes.
var cp1252 = map[rune]byte{
	'€': 0x80, '‚': 0x82, 'ƒ': 0x83, '„': 0x84, '…': 0x85, '†': 0x86, '‡': 0x87, 'ˆ': 0x88,
	'‰': 0x89, 'Š': 0x8a, '‹': 0x8b, 'Œ': 0x8c, 'Ž': 0x8e, '‘': 0x91, '’': 0x92, '“': 0x93,
	'”': 0x94, '•': 0x95, '–': 0x96, '—': 0x97, '˜': 0x98, '™': 0x99, 'š': 0x9a, '›': 0x9b,
	'œ': 0x9c, 'ž': 0x9e, 'Ÿ': 0x9f,
}

// fixMojibake undoes UTF-8 that was decoded as Windows-1252/Latin-1: if
// every character maps back to a single byte and those bytes are valid
// UTF-8 containing multi-byte characters, the decoded text is the intended
// one. Anything else (e.g. a genuine "SÃO") is left alone.
func fixMojibake(s string) string {
	buf := make([]byte, 0, len(s))
	multi := false
	for _, r := range s {
		switch {
		case r < 0x80:
			buf = append(buf, byte(r))
		case r <= 0xff:
			buf = append(buf, byte(r))
			multi = true
		default:
			c, ok := cp1252[r]
			if !ok {
				return s
			}
			buf = append(buf, c)
			multi = true
		}
	}
	if !multi || !utf8.Valid(buf) {
		return s
	}
	return string(buf)
}
