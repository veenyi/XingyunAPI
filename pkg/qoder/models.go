package qoder

// 模型表：静态模型 + 动态目录缓存 + std↔custom 名称映射。

import (
	"strings"
	"sync"
	"time"
)

// StaticModels 是内置静态模型（strings-notes §4）。
var StaticModels = []string{
	"qmodel_latest", "qwen3.7-flash", "qwen3.6-flash",
	"qmodel_38max",  // qwen3.8-max
	"qmodel",        // qwen3.7-plus
	"q37fmodel",
	"q36fmodel",
	"dmodel",        // deepseek-v4-pro
	"dfmodel",       // deepseek-v4-flash
	"gmodel",        // glm-5.3
	"automodel",     // auto
}

// ModelEntry 是对外的模型表项：Name 对外暴露，Upstream 发往上游。
type ModelEntry struct {
	Name     string `json:"name"`
	Upstream string `json:"upstream"`
	Dynamic  bool   `json:"dynamic,omitempty"`
}

// DynamicModelEntry 是动态目录里的一条模型。
type DynamicModelEntry struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Label string `json:"label,omitempty"`
}

// staticModelKeys 是静态模型的规范名集合。
var staticModelKeys = func() map[string]bool {
	m := make(map[string]bool, len(StaticModels))
	for _, k := range StaticModels {
		m[NormalizeModelName(k)] = true
	}
	return m
}()

// stdToCustom 将标准模型名映射为 Qoder 上游使用的自定义名
// （值未从二进制还原，静态三项取同名，另补常用别名；见推断清单）。
var stdToCustom = map[string]string{
	"qmodel_latest": "qmodel_latest",
	"qwen3.7-flash": "qwen3.7-flash",
	"qwen3.6-flash": "qwen3.6-flash",
	"qwen-flash":    "qwen3.7-flash",
	"latest":        "qmodel_latest",
	"qwen3.8-max":   "qmodel_38max",
	"qwen3.7-plus":  "qmodel",
	"deepseek-v4-pro":   "dmodel",
	"deepseek-v4-flash": "dfmodel",
	"glm-5.3":           "gmodel",
	"auto":              "automodel",
}

// customToStd 是 stdToCustom 的反向映射。
var customToStd = func() map[string]string {
	m := make(map[string]string, len(stdToCustom))
	for k, v := range stdToCustom {
		if _, exists := m[v]; !exists {
			m[v] = k
		}
	}
	return m
}()

// NormalizeModelName 规范化模型名：去渠道前缀（"qoder/xxx"）、去空白、转小写。
func NormalizeModelName(name string) string {
	n := strings.TrimSpace(name)
	if i := strings.Index(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	return strings.ToLower(strings.TrimSpace(n))
}

// dynamicModelsCache 是动态模型目录的进程内缓存。
type dynamicModelsCache struct {
	mu    sync.Mutex
	items []DynamicModelEntry
	at    time.Time
}

// get 返回缓存副本与抓取时间。
func (c *dynamicModelsCache) get() []DynamicModelEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]DynamicModelEntry(nil), c.items...)
}

// set 覆盖缓存。
func (c *dynamicModelsCache) set(items []DynamicModelEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = append([]DynamicModelEntry(nil), items...)
	c.at = time.Now()
}

// stale 报告缓存是否超过 ttl。
func (c *dynamicModelsCache) stale(ttl time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.items == nil || time.Since(c.at) > ttl
}

// dynModels 是包级动态模型缓存。
var dynModels = &dynamicModelsCache{}

// dynamicModel 在动态缓存里按规范名查找模型。
func dynamicModel(name string) (DynamicModelEntry, bool) {
	key := NormalizeModelName(name)
	for _, m := range dynModels.get() {
		if NormalizeModelName(m.ID) == key || NormalizeModelName(m.Name) == key {
			return m, true
		}
	}
	return DynamicModelEntry{}, false
}
