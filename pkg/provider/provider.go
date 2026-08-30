// Package provider 定义 handler 需要的上游最小面，让 JoyCode 之外的渠道能并存。
package provider

import (
	"net/http"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/health"
	"github.com/vibe-coding-labs/JoyCode2Api/pkg/joycode"
)

// Chat 是一次对话所需的上游能力。
type Chat interface {
	Name() string
	ListModels() ([]joycode.ModelInfo, error)
	Chat(body map[string]interface{}) (map[string]interface{}, error)
	ChatStream(body map[string]interface{}) (*http.Response, error)
}

// Keyless 表示"不需要用户凭据就能用"的上游。handler 只认这个接口，
// 不 import 具体渠道包：命中它的模型要保留用户点名的模型名，
// 且不参与账号激活、按账号配额兜底这类付费上游逻辑。
type Keyless interface {
	Chat
	// Enabled 每次调用都反映当前设置，开关不需要重启进程。
	Enabled() bool
	// Supports 判定该模型此刻是否由本渠道提供且可用。
	Supports(model string) bool
}

// ErrorClassifier 让每个上游自己解释"这次失败算什么"。
// 冷却档位必须同时看状态码和文本才判得准（不少上游把 429 包在 200 的 JSON 里），
// 而错误文案的格式只有渠道自己知道，所以路由层不猜，只问。
// 未实现该接口的渠道由路由层按错误文本兜底。
type ErrorClassifier interface {
	ClassifyError(err error) (status int, class health.Class, detail string)
}

// 计费边界：自动切换绝不能把一次请求换到另一个人的钱包上。
// 实测踩过：点名要付费模型、该渠道 Key 失效被冷却，路由却换成了免费模型并成功返回，
// 用户以为花的是自己的钱，实际用的是别人的免费额度。
const (
	// TierAccount 花用户登录的行云账号订阅额度。
	TierAccount = "account"
	// TierKeyed 花用户自己填的上游 API Key。
	TierKeyed = "keyed"
	// TierFree 免登录公共渠道，不消耗任何人的额度。
	TierFree = "free"
)

// Tiered 让渠道自己声明归属的计费边界。路由只认这个声明：
// 未实现的渠道按渠道名自成一边界，宁可切不动也不跨边界切。
type Tiered interface {
	Tier() string
}
