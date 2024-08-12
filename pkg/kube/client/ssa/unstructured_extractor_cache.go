// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package ssa

import (
	"fmt"
	"github.com/crossplane-contrib/provider-kubernetes/apis/v1alpha1"
	applymetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/pkg/errors"
)

const (
	errCreateDiscoveryClient = "cannot create discovery client"
	errCreateSSAExtractor    = "cannot create new unstructured server side apply extractor"
	cacheDefaultMaxSize      = 100
)

// ExtractorCacheOption lets you configure a *ExtractorCache.
type ExtractorCacheOption func(cache *ExtractorCache)

// WithCacheMaxSize lets you override the default MaxSize for the cache store.
func WithCacheMaxSize(n int) ExtractorCacheOption {
	return func(c *ExtractorCache) {
		c.maxSize = n
	}
}

// WithCacheStore lets you bootstrap ExtractorCache with your own cache.
func WithCacheStore(cache map[string]*ExtractorCacheEntry) ExtractorCacheOption {
	return func(c *ExtractorCache) {
		c.cache = cache
	}
}

// WithCacheLogger lets you configure the logger for the cache.
func WithCacheLogger(l logging.Logger) ExtractorCacheOption {
	return func(c *ExtractorCache) {
		c.logger = l
	}
}

// NewExtractorCache returns a new empty *ExtractorCache.
func NewExtractorCache(opts ...ExtractorCacheOption) *ExtractorCache {
	c := &ExtractorCache{
		cache:   map[string]*ExtractorCacheEntry{},
		maxSize: cacheDefaultMaxSize,
		mu:      &sync.RWMutex{},
		logger:  logging.NewNopLogger(),
	}
	for _, f := range opts {
		f(c)
	}
	return c
}

// ExtractorCache holds v1.UnstructuredExtractor objects in memory
// so that we don't need to rebuild GVK parser cache at every reconciliation of
// every resource. It has a maximum size that when it's reached, the entry
//
//	that has the oldest access time will be removed from the cache,
//	i.e. FIFO on last access time.
//
// Note that there is no need to invalidate the values in the cache because
// they never change, so we don't need concurrency-safety to prevent access
// to an invalidated entry.
type ExtractorCache struct {
	// cache holds the ExtractorCacheEntry per provider configuration.
	// The cache key is the UID of the provider config object.
	cache map[string]*ExtractorCacheEntry

	// maxSize is the maximum number of elements this cache can ever have.
	maxSize int

	// mu is used to make sure the cache map is concurrency-safe.
	mu *sync.RWMutex

	// logger is the logger for cache operations.
	logger logging.Logger
}

// ExtractorCacheEntry holds the cached v1.UnstructuredExtractor instance and the hash sum
// of the associated ProviderConfig content.
type ExtractorCacheEntry struct {
	sum        string
	dc         discovery.DiscoveryInterface
	extractor  applymetav1.UnstructuredExtractor
	accessedAt atomic.Value
}

// RetrieveApplyExtractor returns the cached v1.UnstructuredExtractor for the given provider config
// if the provider config did not change, otherwise rebuilds the discovery client and v1.UnstructuredExtractor
// the implementation is concurrency-safe.
func (c *ExtractorCache) RetrieveApplyExtractor(pc *v1alpha1.ProviderConfig, specHash string, rc *rest.Config) (applymetav1.UnstructuredExtractor, error) {
	// include generation for spec changes in manifest
	cacheEntrySum := fmt.Sprintf("%d%s", pc.Generation, specHash)
	c.mu.RLock()
	// cache by provider config UID, and overwrite the entry if the content changes
	// invalidated entry due to spec change will be GC'ed
	// TODO(erhan): deleted provider config entries are not explicitly invalidated/removed
	// they will never be used, though will occupy memory
	cacheEntry, ok := c.cache[string(pc.GetUID())]
	c.mu.RUnlock()
	if ok && cacheEntry.sum == cacheEntrySum {
		c.logger.Debug("Cache hit", "cacheEntrySum", cacheEntrySum, "pc", pc.GroupVersionKind().String())
		if time.Since(cacheEntry.accessedAt.Load().(time.Time)) > 10*time.Minute {
			cacheEntry.accessedAt.Store(time.Now())
		}
		return cacheEntry.extractor, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// recheck cache as it might have been already populated
	cacheEntry, ok = c.cache[string(pc.GetUID())]
	if !ok || cacheEntry.sum != cacheEntrySum {
		// cache miss
		c.logger.Debug("Cache miss", "cacheEntrySum", cacheEntrySum, "pc", pc.GetName())
		c.makeRoom()
		dc, err := discovery.NewDiscoveryClientForConfig(rc)
		if err != nil {
			return nil, errors.Wrap(err, errCreateDiscoveryClient)
		}
		applyExtractor, err := applymetav1.NewUnstructuredExtractor(dc)
		if err != nil {
			return nil, errors.Wrap(err, errCreateSSAExtractor)
		}

		cacheEntry = &ExtractorCacheEntry{
			extractor: applyExtractor,
			sum:       cacheEntrySum,
		}
		cacheEntry.accessedAt.Store(time.Now())
		c.cache[string(pc.GetUID())] = cacheEntry
	}
	return cacheEntry.extractor, nil
}

// makeRoom ensures that there is at most maxSize-1 elements in the cache map
// so that a new entry can be added. It deletes the object that
// was last accessed before all others.
// This implementation is not thread safe. Callers must properly synchronize.
func (c *ExtractorCache) makeRoom() {
	if 1+len(c.cache) <= c.maxSize {
		return
	}
	var dustiest string
	for key, val := range c.cache {
		if dustiest == "" {
			dustiest = key
			continue
		}
		if val.accessedAt.Load().(time.Time).Before(c.cache[dustiest].accessedAt.Load().(time.Time)) {
			dustiest = key
		}
	}
	delete(c.cache, dustiest)
}
