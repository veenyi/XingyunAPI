// constants.go WorkBuddy 静态模型兜底表。
package workbuddy

import "github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"

// StaticModels 返回静态兜底模型列表（动态拉取失败时使用）。
func StaticModels() []joycode.ModelInfo {
	names := []string{
		"auto",
		"qwen3.8-max",
		"qwen3.7-max",
		"qwen3.7-plus",
		"deepseek-v4-pro",
		"deepseek-v4-flash",
		"glm-5.3",
		"kimi-k2.7-code",
	}
	out := make([]joycode.ModelInfo, 0, len(names))
	for _, n := range names {
		out = append(out, joycode.ModelInfo{
			Label:          n,
			ChatAPIModel:   n,
			ModelID:        n,
			MaxTotalTokens: 180000,
			SupportStream:  true,
			Provider:       "workbuddy",
		})
	}
	return out
}
