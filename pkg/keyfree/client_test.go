package keyfree

import (
	"strings"
	"testing"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/provider"
)

type fakeSettings map[string]string

func (f fakeSettings) GetSetting(key string) string { return f[key] }

// TestVerifiedListIsVisible 实测名单里的模型必须对外可见，
// 否则设置页打开了开关却一个模型都测不到。
func TestVerifiedListIsVisible(t *testing.T) {
	if len(verifiedModels) == 0 {
		t.Fatal("实测名单为空，免 key 渠道会整体消失")
	}
	for _, m := range verifiedModels {
		if !visible(m) {
			t.Fatalf("实测可用名单里的 %s 应可见", m)
		}
		if strings.TrimSpace(m) == "" {
			t.Fatalf("实测名单里混进了空白模型名: %#v", verifiedModels)
		}
	}
	// 上游目录里的模型名大小写不稳定，判定不能只看原样拼写。
	if !visible(strings.ToUpper(verifiedModels[0])) {
		t.Fatalf("%s 的大写形式也应判为可见", verifiedModels[0])
	}
}

// TestExcludedStayHidden 被排除的模型一个都不能漏出去：
// 它们实测就是区域封锁 / 已下架 / 常年 429，放行等于把报错推给用户。
func TestExcludedStayHidden(t *testing.T) {
	if len(excludedModels) == 0 {
		t.Fatal("排除名单为空说明实测结论丢了，白名单机制会退化成全量放行")
	}
	for m := range excludedModels {
		if visible(m) {
			t.Fatalf("排除名单里的 %s 不该可见", m)
		}
		if strings.TrimSpace(excludedModels[m]) == "" {
			t.Fatalf("被排除的 %s 没写原因，看板无法解释为什么看不到它", m)
		}
		// 两份名单互斥，否则排除表会被实测表重新放行。
		for _, v := range verifiedModels {
			if strings.EqualFold(v, m) {
				t.Fatalf("%s 同时在实测可用与被排除名单里", m)
			}
		}
	}
}

// TestExcludedModelsStayHidden 动态发现后名单随上游目录实时变化（v0.6.2 起
// 不再硬编码固定名单），但实测不可用的模型必须始终被排除；
// 健康冷却由 compat/health 层收缩可见名单，这里只验证排除表。
func TestExcludedModelsStayHidden(t *testing.T) {
	for _, m := range []string{"deepseek-v4-flash-free", "mimo-v2.5-free", "big-pickle", "", "   "} {
		if visible(m) {
			t.Fatalf("实测不可用的 %q 必须保持隐藏", m)
		}
	}
}

func TestEnabledFollowsSetting(t *testing.T) {
	if New(fakeSettings{}, "test", nil).Enabled() {
		t.Fatal("没写过开关的老安装必须保持关闭")
	}
	if New(fakeSettings{SettingEnabled: "0"}, "test", nil).Enabled() {
		t.Fatal("显式关闭后不该是开")
	}
	for _, v := range []string{"1", "true", "TRUE"} {
		if !New(fakeSettings{SettingEnabled: v}, "test", nil).Enabled() {
			t.Fatalf("开关值为 %q 时应为开", v)
		}
	}
}

// TestSettingIsNilSafe 看板在装完但一次都没保存过时设置源可能是空的，
// 这时候 panic 会把整个进程带走。
func TestSettingIsNilSafe(t *testing.T) {
	if got := setting(nil, SettingBaseURL); got != "" {
		t.Fatalf("nil 设置源应给空串，得到 %q", got)
	}
	if got := setting(fakeSettings{SettingBaseURL: "  https://x.example/v1/  "}, SettingBaseURL); got != "https://x.example/v1/" {
		t.Fatalf("应去掉首尾空白，得到 %q", got)
	}
	if New(nil, "test", nil).Enabled() {
		t.Fatal("没有设置源时应视为关闭")
	}
}

// TestDefaultsArePublicHTTPS base URL 兜底值必须是公网 https，
// 否则内网探测会被 netguard 全部拒掉，表现为"开关打开了但没有模型"。
func TestDefaultsArePublicHTTPS(t *testing.T) {
	if !strings.HasPrefix(DefaultBaseURL, "https://") {
		t.Fatalf("默认地址必须是 https，得到 %s", DefaultBaseURL)
	}
	if ProviderName == "" || ProviderName == "joycode" {
		t.Fatalf("渠道名不能与 joycode 撞名: %q", ProviderName)
	}
}

// TestTierIsFree 路由按这个声明决定"能不能互切"。漏了它，免费请求就可能被
// 自动切换推到用户订阅账号或自带 Key 的付费渠道上，账单会算到别人头上。
func TestTierIsFree(t *testing.T) {
	if got := New(fakeSettings{}, "test", nil).Tier(); got != provider.TierFree {
		t.Fatalf("免登录渠道必须声明 free，得到 %q", got)
	}
}
