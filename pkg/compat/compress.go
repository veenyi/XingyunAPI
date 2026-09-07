package compat

import (
	"fmt"
	"regexp"
	"strings"
)

// Context compression for tight free tiers: oversized tool outputs (logs,
// git diffs, grep/ls/tree dumps) are truncated before they are sent upstream,
// keeping the conversation under small token budgets.

const (
	maxTextLen     = 24_000 // per-message content ceiling for detected dumps
	maxAnyTextLen  = 96_000 // absolute ceiling for any single content string
	truncateNotice = "…[行云已截断过长的 %s 输出以适配上下文长度]"
)

var (
	dedupLogRe  = regexp.MustCompile(`(?m)^(DEBUG|INFO|WARN|ERROR|TRACE|FATAL)\b`)
	gitDiffRe   = regexp.MustCompile(`(?m)^(diff --git|@@ -|index [0-9a-f]+\.\.)`)
	grepRe      = regexp.MustCompile(`(?m)^[^:\n]{1,200}:\d+:`)
	lsOutputRe  = regexp.MustCompile(`(?m)^[-dlbcps][rwxsStT-]{9}[\s@+]`)
	treeRe      = regexp.MustCompile(`(?m)^[│├└─\s]*[├└]──\s`)
)

func isDedupLog(s string) bool  { return len(dedupLogRe.FindAllStringIndex(s, 8)) >= 8 }
func isGitDiff(s string) bool   { return len(gitDiffRe.FindAllStringIndex(s, 4)) >= 4 }
func isGrepOutput(s string) bool { return len(grepRe.FindAllStringIndex(s, 12)) >= 12 }
func isLsOutput(s string) bool  { return len(lsOutputRe.FindAllStringIndex(s, 8)) >= 8 }
func isTreeOutput(s string) bool { return len(treeRe.FindAllStringIndex(s, 10)) >= 10 }

func smartTruncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	head := limit * 2 / 3
	tail := limit - head
	return s[:head] + fmt.Sprintf(truncateNotice, "工具") + s[len(s)-tail:]
}

func compressText(s string) string {
	if len(s) <= maxAnyTextLen {
		return s
	}
	return smartTruncate(s, maxAnyTextLen)
}

// CompressText applies dump detection + truncation to one string.
func CompressText(s string) string {
	if len(s) <= maxTextLen {
		return compressText(s)
	}
	switch {
	case isDedupLog(s):
		return smartTruncate(s, maxTextLen)
	case isGitDiff(s):
		return smartTruncate(s, maxTextLen)
	case isGrepOutput(s):
		return smartTruncate(s, maxTextLen)
	case isLsOutput(s):
		return smartTruncate(s, maxTextLen)
	case isTreeOutput(s):
		return smartTruncate(s, maxTextLen)
	}
	return compressText(s)
}

// CompressMessages walks an OpenAI chat body and compresses oversized string
// contents in-place. Returns the same body for chaining.
func CompressMessages(body map[string]interface{}) map[string]interface{} {
	if body == nil {
		return body
	}
	msgs, ok := body["messages"].([]interface{})
	if !ok {
		return body
	}
	for _, m := range msgs {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		switch c := msg["content"].(type) {
		case string:
			msg["content"] = CompressText(c)
		case []interface{}:
			for _, part := range c {
				pm, ok := part.(map[string]interface{})
				if !ok {
					continue
				}
				if t, ok := pm["text"].(string); ok {
					pm["text"] = CompressText(t)
				}
			}
		}
	}
	return body
}

// CompressGitDiff is exported for reuse by channel packages that want to
// squash diff-only messages more aggressively.
func CompressGitDiff(s string) string {
	if isGitDiff(s) {
		return smartTruncate(s, maxTextLen)
	}
	return strings.TrimSpace(s)
}
