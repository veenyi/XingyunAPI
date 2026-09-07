package qoder

// 协议/加密原语（strings-notes §7）：PKCE、AES-CBC、规范化 JSON、嵌套 SSE。
//
// 注意：serverPubKey 未从二进制还原出具体值，定义为包级变量并留空。
// 为空时依赖它的加密步骤（aesCBCEncrypt 及登录载荷签名加密）整体跳过，
// 登录/刷新走未加密字段，行为优雅降级而不是报错。

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
)

// serverPubKey 是 Qoder 登录协议的服务端 RSA 公钥（PEM/未还原，见重建报告）。
// 为空时跳过依赖它的加密步骤。
var serverPubKey = ""

// errServerKeyMissing 在需要公钥但公钥未配置时返回。
var errServerKeyMissing = errors.New("serverPubKey 未配置，跳过加密步骤")

// pkceAlphabet 是 RFC3986 未保留字符集（与 Qoder 客户端 verifier 生成器一致）。
const pkceAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

// makePKCE 生成一对 (code_verifier, code_challenge=S256(verifier))。
// verifier 取 64 个未保留字符（对齐客户端 pde()），challenge 为 base64url(SHA256)。
func makePKCE() (verifier, challenge string) {
	raw := make([]byte, 64)
	_, _ = rand.Read(raw)
	var b strings.Builder
	b.Grow(len(raw))
	for _, c := range raw {
		b.WriteByte(pkceAlphabet[int(c)%len(pkceAlphabet)])
	}
	verifier = b.String()
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// pkcs7Pad 按块大小做 PKCS#7 填充。
func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	out := make([]byte, len(data)+pad)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(pad)
	}
	return out
}

// aesCBCEncrypt 用 key 做 AES-CBC 加密（随机 IV 前置），输出 qoderEncode 编码。
// key 为空（如 serverPubKey 未还原出的派生密钥缺失）时返回 ("", nil)，
// 调用方据此跳过该字段——与二进制的优雅降级行为一致。
func aesCBCEncrypt(key, plaintext []byte) (string, error) {
	if len(key) == 0 {
		return "", nil
	}
	if len(plaintext) == 0 {
		return "", nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	padded := pkcs7Pad(plaintext, block.BlockSize())
	out := make([]byte, block.BlockSize()+len(padded))
	iv := out[:block.BlockSize()]
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", err
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out[block.BlockSize():], padded)
	return qoderEncode(out), nil
}

// qoderEncode 是协议使用的 base64 变体：标准字母表、无填充。
func qoderEncode(b []byte) string {
	return base64.StdEncoding.WithPadding(base64.NoPadding).EncodeToString(b)
}

// jsonSortedCompact 输出规范化 JSON：对象键递归排序后紧凑序列化，
// 用于签名（canonical JSON）。数字经 json.Number 保真，不发生浮点重排。
func jsonSortedCompact(v interface{}) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree interface{}
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := encodeSorted(&buf, tree); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeSorted(buf *bytes.Buffer, v interface{}) error {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			if err := encodeSorted(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []interface{}:
		buf.WriteByte('[')
		for i, it := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeSorted(buf, it); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf.Write(b)
	}
	return nil
}

// parseNestedSSE 解析「嵌套 SSE」：上游事件 data 载荷里内嵌了一层 SSE 文本
// （形如 `data: {...}\n\n`），返回内层全部 data 载荷（丢弃 [DONE]/空行）。
func parseNestedSSE(payload string) []string {
	var out []string
	for _, line := range strings.Split(payload, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		s := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if s == "" || s == "[DONE]" {
			continue
		}
		out = append(out, s)
	}
	return out
}
