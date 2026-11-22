package engine

import (
	"context"
	"errors"
	"sync"

	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
)

// queryCacheSize is how many query vectors stay in memory; the oldest goes first.
const queryCacheSize = 512

// queryCache is the query embeddings an engine keeps in memory: the latest queryCacheSize vectors, the
// embeddings under way (so callers asking for the same query share one call), and which of the kept vectors
// the provider made and the database does not have yet. Its zero value is ready to use.
//
// mu is a leaf lock (batch.go lists the engine's lock order): no other engine lock is taken while it is
// held, and no method here calls out of the cache.
type queryCache struct {
	mu      sync.Mutex
	vectors map[string][]float32
	order   []string                // keys of vectors, oldest first
	flights map[string]*queryFlight // query embeddings under way
	unsaved map[string]bool         // in vectors, not yet in the database
}

// join returns key's vector when it is kept. Otherwise it returns the embedding under way for key; when
// there is none it starts one with failed as its result until the caller sets another, and own is true:
// the caller makes the embedding and calls land when it is done.
func (c *queryCache) join(key string, failed error) (v []float32, f *queryFlight, own bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.vectors[key]; ok {
		return v, nil, false
	}
	if f, ok := c.flights[key]; ok {
		return nil, f, false
	}
	f = &queryFlight{done: make(chan struct{}), err: failed}
	if c.flights == nil {
		c.flights = map[string]*queryFlight{}
	}
	c.flights[key] = f
	return nil, f, true
}

// land forgets the embedding under way for key; the next caller starts its own.
func (c *queryCache) land(key string) {
	c.mu.Lock()
	delete(c.flights, key)
	c.mu.Unlock()
}

// markUnsaved notes that key's vector came from the provider and is not in the database yet.
func (c *queryCache) markUnsaved(key string) {
	c.mu.Lock()
	if c.unsaved == nil {
		c.unsaved = map[string]bool{}
	}
	c.unsaved[key] = true
	c.mu.Unlock()
}

// takeUnsaved reports whether key's vector still has to be written to the database, and clears the mark:
// one caller writes it.
func (c *queryCache) takeUnsaved(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	unsaved := c.unsaved[key]
	delete(c.unsaved, key)
	return unsaved
}

// remember keeps v under key, pushing out the oldest vectors (and their unsaved marks) past queryCacheSize.
func (c *queryCache) remember(key string, v []float32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.vectors == nil {
		c.vectors = map[string][]float32{}
	}
	if _, ok := c.vectors[key]; !ok {
		c.order = append(c.order, key)
	}
	c.vectors[key] = v
	for len(c.order) > queryCacheSize {
		delete(c.vectors, c.order[0])
		delete(c.unsaved, c.order[0])
		c.order = c.order[1:]
	}
}

// has reports whether key's vector is kept.
func (c *queryCache) has(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.vectors[key]
	return ok
}

// EmbedQuery returns a normalized query vector (implements search.QueryEmbedder).
func (e *Engine) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	key := e.queryKey(text)
	v, err := e.queryVector(ctx, key, text)
	if err != nil {
		return nil, err
	}
	e.keepQuery(key, v)
	return append([]float32(nil), v...), nil
}

// WarmQuery starts or joins the embedding of a query a search may ask for next (chat.QueryWarmer). The
// vector stays in memory only: it reaches the query cache in the database when a search asks for it, so
// a message nobody searched with leaves nothing behind. A failure is the search's to meet again.
func (e *Engine) WarmQuery(ctx context.Context, text string) {
	if ix := e.Index(); ix == nil || ix.Recipe != e.Settings.Recipe.Version() || e.Store.DB.HasPendingEvents("source-change") {
		return // the search would not run (ErrNotReady)
	}
	_, _ = e.queryVector(ctx, e.queryKey(text), text)
}

// queryKey is keyed by space and stored at its dimensionality: a quarter of the size at 1024 of 4096 dims.
func (e *Engine) queryKey(text string) string {
	return store.CacheKey(e.EmbedSpace(), "query:"+e.Settings.QueryInstruction, text)
}

// queryFlight is one query embedding under way. Callers asking for the same query wait for it instead of
// paying for another. v and err are set before done closes.
type queryFlight struct {
	done chan struct{}
	v    []float32
	err  error
}

// queryVector is the query's vector from memory, the database or the provider; the slice is shared, not to
// be changed. A caller that finds the same embedding under way waits for it, as long as its own ctx lasts.
// If that one fails or its caller gave it up, the waiting caller tries for itself: each keeps the attempt
// it would have had alone.
func (e *Engine) queryVector(ctx context.Context, key, text string) ([]float32, error) {
	for {
		v, f, own := e.queries.join(key, errors.New("memory: query embedding did not finish"))
		if f == nil {
			return v, nil
		}
		if own {
			return e.flyQuery(ctx, key, text, f)
		}
		select {
		case <-f.done:
			if f.err == nil {
				return f.v, nil
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (e *Engine) flyQuery(ctx context.Context, key, text string, f *queryFlight) ([]float32, error) {
	defer func() {
		e.queries.land(key)
		close(f.done)
	}()
	if raw, ok := e.Store.CachedVector(key); ok {
		v := search.DecodeFloat16(raw, nil)
		e.queries.remember(key, v)
		f.v, f.err = v, nil
		return v, nil
	}
	if e.LLM == nil || !e.LLM.Configured() {
		f.err = errors.New("memory: embedding provider not configured")
		return nil, f.err
	}
	v, err := e.hedgedEmbed(ctx, e.queryInput(text))
	if err != nil {
		f.err = err
		return nil, err
	}
	v = e.fitDims(v)
	search.Normalize(v)
	e.queries.markUnsaved(key)
	e.queries.remember(key, v)
	f.v, f.err = v, nil
	return v, nil
}

// keepQuery writes a vector the provider made to the query cache in the database, once.
func (e *Engine) keepQuery(key string, v []float32) {
	if e.queries.takeUnsaved(key) {
		_ = e.Store.PutQueryVectors(map[string][]byte{key: search.EncodeFloat16(v)})
	}
}
