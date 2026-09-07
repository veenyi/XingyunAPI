package workbuddy

import (
	"encoding/json"
	"testing"
)

type mapStore struct{ m map[string]string }

func (s *mapStore) GetSetting(k string) string        { return s.m[k] }
func (s *mapStore) SetSetting(k, v string) error      { s.m[k] = v; return nil }

func checkinBlob(accounts ...map[string]interface{}) string {
	arr := []interface{}{}
	for _, a := range accounts {
		arr = append(arr, map[string]interface{}(a))
	}
	b, _ := json.Marshal(map[string]interface{}{"accounts": arr})
	return string(b)
}

func TestBackfillCheckinAccounts(t *testing.T) {
	st := &mapStore{m: map[string]string{}}
	blob := checkinBlob(
		map[string]interface{}{"platform": "workbuddy", "uid": "u-wb-1", "name": "甲", "enabled": true,
			"enc_access": "AA01", "enc_refresh": "BB01"},
		map[string]interface{}{"platform": "workbuddy", "uid": "u-wb-2", "name": "乙", "enabled": false,
			"enc_access": "AA02"},
		map[string]interface{}{"platform": "workbuddy", "uid": "u-wb-3", "name": "丙", "enabled": true},
		map[string]interface{}{"platform": "qoder", "uid": "u-q-1", "name": "丁", "enabled": true,
			"enc_access": "AA04"},
	)
	if got := BackfillCheckinAccounts(st, blob); got != 1 {
		t.Fatalf("imported=%d want 1 (disabled/tokenless/foreign rows must skip)", got)
	}
	var items []storedAccount
	if err := json.Unmarshal([]byte(st.m[SettingsKey]), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].UserID != "u-wb-1" || items[0].EncAccess != "AA01" || items[0].Nickname != "甲" {
		t.Fatalf("unexpected blob: %+v", items)
	}
	// 幂等：重复回填不再新增
	if got := BackfillCheckinAccounts(st, blob); got != 0 {
		t.Fatalf("second run imported=%d want 0", got)
	}
}

func TestBackfillExistingRowWins(t *testing.T) {
	st := &mapStore{m: map[string]string{}}
	existing := []storedAccount{{UserID: "u-wb-1", Nickname: "旧名", EncAccess: "KEEP"}}
	b, _ := json.Marshal(existing)
	st.m[SettingsKey] = string(b)
	blob := checkinBlob(map[string]interface{}{"platform": "workbuddy", "uid": "u-wb-1", "name": "新名",
		"enabled": true, "enc_access": "NEW"})
	if got := BackfillCheckinAccounts(st, blob); got != 0 {
		t.Fatalf("imported=%d want 0 (existing row must win)", got)
	}
	var items []storedAccount
	if err := json.Unmarshal([]byte(st.m[SettingsKey]), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].EncAccess != "KEEP" {
		t.Fatalf("existing blob was mutated: %+v", items)
	}
}

func TestBackfillCorruptBlobNoTouch(t *testing.T) {
	st := &mapStore{m: map[string]string{SettingsKey: "not-json"}}
	if got := BackfillCheckinAccounts(st, checkinBlob(map[string]interface{}{"platform": "workbuddy",
		"uid": "u1", "enc_access": "X"})); got != 0 {
		t.Fatalf("imported=%d want 0 on corrupt existing blob", got)
	}
	if st.m[SettingsKey] != "not-json" {
		t.Fatal("corrupt existing blob must stay untouched")
	}
	st2 := &mapStore{m: map[string]string{}}
	if got := BackfillCheckinAccounts(st2, "}{"); got != 0 {
		t.Fatalf("imported=%d want 0 on corrupt checkin blob", got)
	}
	if _, ok := st2.m[SettingsKey]; ok {
		t.Fatal("no blob must be written when checkin blob is corrupt")
	}
}
