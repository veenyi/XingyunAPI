package store

import (
	"strings"
	"testing"
)

func TestValidAPIKeyAggregateKey(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetSecretSetting("aggregate_key", "sk-joy-test-aggregate"); err != nil {
		t.Fatal(err)
	}
	if !s.ValidAPIKey("sk-joy-test-aggregate") {
		t.Error("aggregate key should validate")
	}
	if s.ValidAPIKey("sk-joy-wrong") {
		t.Error("unknown key must not validate")
	}
	if s.ValidAPIKey("") {
		t.Error("empty key must not validate")
	}
}

func TestValidAPIKeyAccountTokenAndUserID(t *testing.T) {
	s := openTestStore(t)
	if err := s.AddAccount("user-a", "pt-a", "nick", false, ""); err != nil {
		t.Fatal(err)
	}
	tok, err := s.GetAccount("user-a")
	if err != nil || tok == nil {
		t.Fatalf("get account: %v", err)
	}
	if tok.APIToken == "" {
		t.Fatal("AddAccount did not provision an api_token")
	}
	if !s.ValidAPIKey(tok.APIToken) {
		t.Error("account api_token should validate")
	}
	if !s.ValidAPIKey("user-a") {
		t.Error("account user_id should validate")
	}
	if s.ValidAPIKey("sk-joy-nope") {
		t.Error("unknown key must not validate")
	}
}

func TestAggregateKeyMigratesPlaintext(t *testing.T) {
	s := openTestStore(t)
	// 模拟历史明文行（r9 旧版直接 SetSetting 写入）
	if err := s.SetSetting("aggregate_key", "sk-joy-legacy-plain"); err != nil {
		t.Fatal(err)
	}
	got := s.AggregateKey()
	if got != "sk-joy-legacy-plain" {
		t.Fatalf("AggregateKey = %q, want legacy plaintext", got)
	}
	// 迁移后应已变为密文（解密成功且与明文不一致）
	raw := s.GetSetting("aggregate_key")
	if raw == "sk-joy-legacy-plain" {
		t.Fatal("aggregate_key row still plaintext after migration")
	}
	dec, err := s.GetSecretSetting("aggregate_key")
	if err != nil || dec != "sk-joy-legacy-plain" {
		t.Fatalf("after migration decrypt = %q, %v", dec, err)
	}
	// 二次读取走密文路径，结果一致
	if again := s.AggregateKey(); again != "sk-joy-legacy-plain" {
		t.Fatalf("second AggregateKey = %q", again)
	}
}

func TestAggregateKeyNonKeyGarbageReturnsEmpty(t *testing.T) {
	s := openTestStore(t)
	if err := s.SetSetting("aggregate_key", "not-a-key-not-hex"); err != nil {
		t.Fatal(err)
	}
	if got := s.AggregateKey(); got != "" {
		t.Fatalf("garbage row should yield empty, got %q", got)
	}
	if strings.ContainsRune(s.GetSetting("aggregate_key"), ' ') == false && s.GetSetting("aggregate_key") == "" {
		t.Fatal("row unexpectedly removed")
	}
}
