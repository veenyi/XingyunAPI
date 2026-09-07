// Package provider defines the channel interfaces that drive model routing.
// A "Chat" provider is the credentialed JoyCode SaaS backend; "Keyless"
// providers are the additional channels (BYO-key presets, key-free pools,
// custom OpenAI-compatible upstreams, Qoder/WorkBuddy account pools).
package provider

import (
	"context"
	"io"
)

// Chat is implemented by the JoyCode SaaS client (pkg/joycode).
type Chat interface {
	Chat(ctx context.Context, body map[string]interface{}) (map[string]interface{}, error)
	ChatStream(ctx context.Context, body map[string]interface{}) (io.ReadCloser, error)
	ListModels() []string
	Name() string
}

// Keyless is the shared method set of every non-JoyCode channel.
// Catalog-capable channels additionally implement Tier, CatalogIDs,
// ModelIDs, ClassifyError and RefreshNow.
type Keyless interface {
	Chat(ctx context.Context, body map[string]interface{}) (map[string]interface{}, error)
	ChatStream(ctx context.Context, body map[string]interface{}) (io.ReadCloser, error)
	Enabled() bool
	ListModels() []string
	Name() string
	Supports(model string) bool
}

// Catalog is the optional richer surface of catalog-capable channels.
type Catalog interface {
	Tier() int
	CatalogIDs() []string
	ModelIDs() []string
	ClassifyError(err error) string
	RefreshNow(ctx context.Context)
}
