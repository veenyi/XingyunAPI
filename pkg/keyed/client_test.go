package keyed

import (
	"strings"
	"testing"

	"github.com/vibe-coding-labs/JoyCode2Api/pkg/provider"
)

type fakeSettings map[string]string

func (f fakeSettings) GetSetting(key string) string { return f[key] }

// fakeSecrets 单独记密钥：密钥与明文设置走的是两条读写路径，
// 混在一个 map 里会让"密钥被当普通设置输出"这类回归看不出来。
type fakeStore struct {
	plain  fakeSettings
	secret map[string]string
}

func (s fakeStore) GetSetting(key string) string { return s.plain[key] }

func (s fakeStore) GetSecretSetting(key string) string { return s.secret[key] }

func TestEnabledRequiresBothSwitchAndKey(t *testing.T) {
	st := fakeStore{plain: fakeSettings{SettingEnabled: "1"}, secret: map[string]string{}}
	c := New(st, "test", nil)
	if c.Enabled() {
		t.Fatal("没填 Key 的自带 Key 渠道不该启用，否则每个请求都注定 401")
	}

	st.secret[SettingAPIKey] = "sk-demo"
	if !c.Enabled() {
		t.Fatal("开关打开且已填 Key 后应为开")
	}

	st.plain[SettingEnabled] = "false"
	if c.Enabled() {
		t.Fatal("关掉了就必须是关")
	}
}

// TestTakesEffectWithoutRestart 文档里承诺"开关不需要重启进程"，
// 这里钉住：判定读的是当前设置，而不是构造时的快照。
func TestTakesEffectWithoutRestart(t *testing.T) {
	plain := fakeSettings{}
	st := fakeStore{plain: plain, secret: map[string]string{SettingAPIKey: "sk-demo"}}
	c := New(st, "test", nil)
	if c.Enabled() {
		t.Fatal("初始未写过开关应为关")
	}
	plain[SettingEnabled] = "true"
	if !c.Enabled() {
		t.Fatal("改设置后同一个客户端实例必须立刻反映新状态")
	}
}

func TestBlankKeyCountsAsNoKey(t *testing.T) {
	st := fakeStore{
		plain:  fakeSettings{SettingEnabled: "1"},
		secret: map[string]string{SettingAPIKey: "   \n "},
	}
	if New(st, "test", nil).Enabled() {
		t.Fatal("只有空白的 Key 等于没填，不该启用渠道")
	}
}

func TestExtraModelsParsing(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"glm-4.7", []string{"glm-4.7"}},
		{"a, b\nc;d ; e", []string{"a", "b", "c", "d", "e"}},
		{"a,,b", []string{"a", "b"}},
	}
	for _, tc := range cases {
		got := extraModels(fakeStore{plain: fakeSettings{SettingExtraMode: tc.raw}, secret: map[string]string{}})
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Fatalf("解析 %q 应得 %v，得到 %v", tc.raw, tc.want, got)
		}
	}
}

func TestNilSettingsSafe(t *testing.T) {
	c := New(nil, "test", nil)
	if c.Enabled() {
		t.Fatal("没有设置源时应视为关闭")
	}
	if got := c.apiKey(); got != "" {
		t.Fatalf("没有设置源时不该有密钥，得到 %q", got)
	}
	if got := setting(nil, SettingBaseURL); got != "" {
		t.Fatalf("nil 设置源应给空串，得到 %q", got)
	}
	// 空实例也不能 panic：真实调用链上存在先构造后赋值的窗口。
	var zero *Client
	if zero.apiKey() != "" {
		t.Fatal("nil 客户端应没有密钥")
	}
}

func TestDefaultsLookSane(t *testing.T) {
	if !strings.HasPrefix(DefaultBaseURL, "https://") {
		t.Fatalf("默认地址必须是 https，得到 %s", DefaultBaseURL)
	}
	if !strings.HasPrefix(NvidiaBaseURL, "https://") {
		t.Fatalf("预置地址必须是 https，得到 %s", NvidiaBaseURL)
	}
	if ProviderName == "" || ProviderName == "joycode" {
		t.Fatalf("渠道名不能与 joycode 撞名: %q", ProviderName)
	}
	// 键名不能互相覆盖，否则保存一个设置会静默改掉另一个。
	keys := []string{SettingEnabled, SettingBaseURL, SettingAPIKey, SettingExtraMode, SettingPreset}
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k] {
			t.Fatalf("设置键名重复: %s", k)
		}
		seen[k] = true
	}
}

// TestPresetPicksBaseURL 预设只决定"没手填地址时打哪儿"，手填永远优先；
// 且每次调用都重读设置，换预设不需要重启进程。
func TestPresetPicksBaseURL(t *testing.T) {
	st := fakeStore{plain: fakeSettings{}, secret: map[string]string{}}
	if got := resolveBaseURL(st); got != DefaultBaseURL {
		t.Fatalf("什么都没填时应回到智谱默认，得到 %s", got)
	}

	st.plain[SettingPreset] = "nvidia"
	if got := resolveBaseURL(st); got != NvidiaBaseURL {
		t.Fatalf("选 nvidia 预设要用 NVIDIA 免费端点，得到 %s", got)
	}

	st.plain[SettingPreset] = " NVIDIA "
	if got := resolveBaseURL(st); got != NvidiaBaseURL {
		t.Fatalf("预设名要容忍大小写和空白，得到 %s", got)
	}

	st.plain[SettingPreset] = "bai"
	if got := resolveBaseURL(st); got != BaiBaseURL {
		t.Fatalf("选 bai 预设要用 b.ai 端点，得到 %s", got)
	}

	st.plain[SettingPreset] = " BAi "
	if got := resolveBaseURL(st); got != BaiBaseURL {
		t.Fatalf("bai 预设也要容忍大小写和空白，得到 %s", got)
	}

	st.plain[SettingPreset] = "nvidia"
	st.plain[SettingBaseURL] = "  "
	if got := resolveBaseURL(st); got != NvidiaBaseURL {
		t.Fatalf("空白地址不该覆盖预设，得到 %s", got)
	}
	st.plain[SettingBaseURL] = "https://my.gateway.example/v1"
	if got := resolveBaseURL(st); got != "https://my.gateway.example/v1" {
		t.Fatalf("手填地址必须优先于预设，得到 %s", got)
	}

	st.plain[SettingPreset] = "没听过的预设"
	st.plain[SettingBaseURL] = ""
	if got := resolveBaseURL(st); got != DefaultBaseURL {
		t.Fatalf("认不出的预设要兜到默认地址而不是空地址，得到 %s", got)
	}
}

func TestTierIsTheUsersOwnWallet(t *testing.T) {
	c := New(fakeStore{plain: fakeSettings{}, secret: map[string]string{}}, "test", nil)
	if got := c.Tier(); got != provider.TierKeyed {
		t.Fatalf("自带 Key 渠道花的是用户自己的钱，得到 %q", got)
	}
	if provider.TierKeyed == provider.TierFree || provider.TierKeyed == provider.TierAccount {
		t.Fatal("三个计费边界必须互不相同，否则路由的边界判定形同虚设")
	}
}
