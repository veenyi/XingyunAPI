// models_cache.go 动态模型列表缓存。
package workbuddy

import "time"

type dynModelsCache struct {
	entries   []DynamicModelEntry
	fetchedAt time.Time
	err       error
	errAt     time.Time
}

const (
	dynModelsTTL     = 1 * time.Hour
	dynModelsFailTTL = 5 * time.Minute
)

func (c *dynModelsCache) get() ([]DynamicModelEntry, bool) {
	now := time.Now()
	if c.entries != nil && now.Sub(c.fetchedAt) < dynModelsTTL {
		return c.entries, true
	}
	if c.err != nil && now.Sub(c.errAt) < dynModelsFailTTL {
		return nil, false
	}
	return nil, false
}

func (c *dynModelsCache) set(entries []DynamicModelEntry, err error) {
	if err != nil {
		c.err = err
		c.errAt = time.Now()
		return
	}
	c.entries = entries
	c.fetchedAt = time.Now()
	c.err = nil
}
