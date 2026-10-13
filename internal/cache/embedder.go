// Package cache implements the two-tier response cache:
//
//	L1  exact-match: sharded in-process LRU  ->  shared Redis
//	L2  semantic:    PostgreSQL pgvector HNSW cosine-similarity search
//
// Prompts are normalized, hashed for the exact tier, and embedded with a
// pluggable Embedder for the semantic tier.
package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"strings"
	"time"
)

// Dim is the embedding dimensionality stored in pgvector (vector(384)).
const Dim = 384

// Embedder turns text into a unit-length vector.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Name() string
}

// HashingEmbedder is a deterministic, dependency-free embedder using signed
// feature hashing over normalized word unigrams, word bigrams, and character
// trigrams. It captures lexical/paraphrase similarity (reordering, casing,
// punctuation, contractions, stop-words, inflections, common synonyms) at
// ~10µs per prompt, so local benchmarks need no external embedding API.
type HashingEmbedder struct {
	dim int
}

// NewHashingEmbedder returns a HashingEmbedder producing Dim-sized vectors.
func NewHashingEmbedder() *HashingEmbedder { return &HashingEmbedder{dim: Dim} }

func (h *HashingEmbedder) Name() string { return "hashing-ngram-384" }

const (
	wUnigram = 1.0
	wBigram  = 0.35
	wTrigram = 0.18
)

// Embed implements Embedder.
func (h *HashingEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	return h.embed(text), nil
}

func (h *HashingEmbedder) embed(text string) []float32 {
	v := make([]float64, h.dim)
	toks := ContentTokens(text)
	for i, t := range toks {
		h.add(v, "u:"+t, wUnigram)
		if i > 0 {
			h.add(v, "b:"+toks[i-1]+"|"+t, wBigram)
		}
		padded := "#" + t + "#"
		if len(padded) >= 3 {
			n := float64(len(padded) - 2)
			for j := 0; j+3 <= len(padded); j++ {
				h.add(v, "c:"+padded[j:j+3], wTrigram/math.Sqrt(n))
			}
		}
	}
	out := make([]float32, h.dim)
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	if norm == 0 {
		// Empty/stop-word-only prompt: stable non-zero vector so cosine
		// distance is defined.
		out[0] = 1
		return out
	}
	inv := 1 / math.Sqrt(norm)
	for i, x := range v {
		out[i] = float32(x * inv)
	}
	return out
}

func (h *HashingEmbedder) add(v []float64, feature string, w float64) {
	f := fnv.New64a()
	_, _ = f.Write([]byte(feature))
	sum := f.Sum64()
	idx := int(sum % uint64(h.dim))
	if sum>>63 == 1 {
		w = -w
	}
	v[idx] += w
}

// Cosine returns the cosine similarity of two unit vectors.
func Cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// HTTPEmbedder calls an OpenAI-compatible /v1/embeddings endpoint, requesting
// Dim dimensions so vectors fit the pgvector column.
type HTTPEmbedder struct {
	URL    string // e.g. https://api.openai.com/v1/embeddings
	Model  string
	APIKey string
	Client *http.Client
}

// NewHTTPEmbedder returns an embedder for an OpenAI-compatible API.
func NewHTTPEmbedder(url, model, apiKey string) *HTTPEmbedder {
	return &HTTPEmbedder{URL: url, Model: model, APIKey: apiKey, Client: &http.Client{Timeout: 5 * time.Second}}
}

func (e *HTTPEmbedder) Name() string { return "http:" + e.Model }

// Embed implements Embedder.
func (e *HTTPEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{"input": text, "model": e.Model, "dimensions": Dim})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding request: status %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embedding decode: %w", err)
	}
	if len(out.Data) == 0 {
		return nil, errors.New("embedding response has no data")
	}
	v := out.Data[0].Embedding
	if len(v) != Dim {
		return nil, fmt.Errorf("embedding has %d dims, want %d", len(v), Dim)
	}
	normalizeInPlace(v)
	return v, nil
}

func normalizeInPlace(v []float32) {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(n))
	for i := range v {
		v[i] *= inv
	}
}

// NormalizeText lower-cases, expands contractions, strips punctuation, and
// collapses whitespace. It defines exact-cache equivalence.
func NormalizeText(s string) string {
	s = strings.ToLower(s)
	s = contractions.Replace(s)
	var b strings.Builder
	b.Grow(len(s))
	space := true
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r > 127 {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

var contractions = strings.NewReplacer(
	"what's", "what is", "how's", "how is", "where's", "where is", "who's", "who is",
	"it's", "it is", "that's", "that is", "there's", "there is", "let's", "let us",
	"can't", "can not", "cannot", "can not", "won't", "will not", "don't", "do not",
	"doesn't", "does not", "isn't", "is not", "aren't", "are not", "i'm", "i am",
	"you're", "you are", "we're", "we are", "they're", "they are", "i've", "i have",
	"'ll", " will", "'d", " would", "’", "'",
)

// ContentTokens returns stemmed, synonym-canonicalized content words.
func ContentTokens(s string) []string {
	words := strings.Fields(NormalizeText(s))
	out := words[:0]
	for _, w := range words {
		if _, stop := stopwords[w]; stop {
			continue
		}
		w = stem(w)
		if c, ok := synonyms[w]; ok {
			w = c
		}
		out = append(out, w)
	}
	return out
}

func stem(w string) string {
	if len(w) <= 4 {
		return w
	}
	for _, suf := range []string{"ational", "ization", "ations", "ation", "ness", "ments", "ment", "ings", "ing", "ies", "ied", "ers", "er", "ed", "es", "ly", "s"} {
		if strings.HasSuffix(w, suf) && len(w)-len(suf) >= 3 {
			base := w[:len(w)-len(suf)]
			switch suf {
			case "ies", "ied":
				return base + "y"
			}
			return base
		}
	}
	return w
}

var stopwords = toSet(`a an the and or but if then else of to in on at by for with about as into
like through over after before between under again further once here there when where why how all
any both each few more most other some such no nor not only own same so than too very s t can will
just should now i me my myself we our ours you your yours he him his she her it its they them their
what which who whom this that these those am is are was were be been being have has had having do
does did doing would could please kindly tell give show me us let could quick quickly briefly simple
simply short some bit little really actually basically hey hi hello thanks thank you can`)

func toSet(s string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, w := range strings.Fields(s) {
		m[w] = struct{}{}
	}
	return m
}

// synonyms canonicalizes common near-synonyms (after stemming) so paraphrases
// that swap verbs/nouns land on the same features.
var synonyms = map[string]string{
	"describ": "explain", "describe": "explain", "elaborat": "explain", "clarify": "explain", "overview": "explain", "explanation": "explain", "walk": "explain",
	"advantag": "benefit", "pro": "benefit", "pros": "benefit", "upside": "benefit", "strength": "benefit",
	"disadvantag": "downside", "drawback": "downside", "con": "downside", "cons": "downside", "weakness": "downside", "limit": "downside", "limitation": "downside",
	"exampl": "example", "sample": "example", "instance": "example", "illustrat": "example",
	"differ": "difference", "differenc": "difference", "different": "difference", "contrast": "difference", "compar": "difference", "compare": "difference", "comparison": "difference", "vs": "difference", "versus": "difference",
	"function": "work", "operat": "work", "operate": "work",
	"fix": "debug", "troubleshoot": "debug", "resolv": "debug", "resolve": "debug",
	"using": "use", "usage": "use", "uses": "use", "utiliz": "use", "employ": "use",
	"build": "create", "make": "create", "writ": "create", "write": "create", "implement": "create", "generat": "create",
	"fast": "performance", "speed": "performance", "perform": "performance", "optimiz": "performance", "faster": "performance",
	"term": "terms", "layman": "terms", "beginner": "terms", "eli5": "terms", "plain": "terms",
}
