package qwenwork

// pbchat：chat.proto 的手写编解码（model.chat.ChatService）。
// 请求：OpenAI body(map) → ChatCompletionRequest（字段号见 chat.proto 1-24）。
// 响应：ChatCompletionChunk → OpenAI chunk(map)。
// Struct/Value（google.protobuf）按 proto 定义手写转换。

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// ---- Struct / Value 编码 ----

// pbStructField 写 google.protobuf.Struct 字段（fields=1 map entry）。
func (w *pbBuf) pbStructField(field int, m map[string]interface{}) {
	w.msg(field, func(s *pbBuf) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s.msg(1, func(e *pbBuf) {
				e.str(1, k)
				e.pbValueField(2, m[k])
			})
		}
	})
}

// pbValueField 写 google.protobuf.Value 字段。
func (w *pbBuf) pbValueField(field int, v interface{}) {
	w.msg(field, func(vb *pbBuf) { pbValueInto(vb, v) })
}

// pbValueInto 把 Go 值写进 Value 消息体（kind oneof：null=1/number=2/string=3/bool=4/struct=5/list=6）。
func pbValueInto(vb *pbBuf, v interface{}) {
	switch t := v.(type) {
	case nil:
		vb.varint(1, 0) // NULL_VALUE = 0（oneof 成员即使 0 也要编码）
	case bool:
		vb.varint(4, boolU64(t))
	case float64:
		vb.double(2, t)
	case float32:
		vb.double(2, float64(t))
	case int:
		vb.double(2, float64(t))
	case int64:
		vb.double(2, float64(t))
	case string:
		vb.str(3, t)
	case []interface{}:
		vb.msg(6, func(lb *pbBuf) {
			for _, item := range t {
				lb.msg(1, func(iv *pbBuf) { pbValueInto(iv, item) })
			}
		})
	case map[string]interface{}:
		vb.pbStructField(5, t)
	default:
		vb.str(3, fmt.Sprint(v))
	}
}

func boolU64(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// ---- 请求编码 ----

// encodeChatRequest 把 OpenAI 兼容 body 编码为 ChatCompletionRequest。
// requestID/sessionID 用于 metadata.context（worker Ygn：context.request_id
// 与 gRPC 头 x-request-id 同值）。
func encodeChatRequest(body map[string]interface{}, stream bool, requestID, sessionID string) []byte {
	w := &pbBuf{}
	if m := fmt.Sprint(body["model"]); m != "" {
		w.str(1, m)
	}
	if msgs, ok := body["messages"].([]interface{}); ok {
		for _, mi := range msgs {
			if mm, ok := mi.(map[string]interface{}); ok {
				encodeChatMessage(w, 2, mm)
			}
		}
	}
	if v, ok := body["temperature"].(float64); ok {
		w.msg(3, func(x *pbBuf) { x.double(1, v) })
	}
	if v, ok := body["top_p"].(float64); ok {
		w.msg(4, func(x *pbBuf) { x.double(1, v) })
	}
	if v, ok := body["max_tokens"].(float64); ok {
		w.msg(5, func(x *pbBuf) { x.varint(1, uint64(int64(v))) })
	}
	w.varint(6, boolU64(stream))
	if stops, ok := body["stop"].([]interface{}); ok {
		for _, s := range stops {
			w.str(7, fmt.Sprint(s))
		}
	} else if s, ok := body["stop"].(string); ok && s != "" {
		w.str(7, s)
	}
	if v, ok := body["presence_penalty"].(float64); ok {
		w.msg(8, func(x *pbBuf) { x.double(1, v) })
	}
	if v, ok := body["frequency_penalty"].(float64); ok {
		w.msg(9, func(x *pbBuf) { x.double(1, v) })
	}
	if v, ok := body["n"].(float64); ok {
		w.msg(10, func(x *pbBuf) { x.varint(1, uint64(int64(v))) })
	}
	if v, ok := body["user"].(string); ok && v != "" {
		w.msg(11, func(x *pbBuf) { x.str(1, v) })
	}
	if v, ok := body["seed"].(float64); ok {
		w.msg(12, func(x *pbBuf) { x.varint(1, uint64(int64(v))) })
	}
	// reasoning_effort：openai 风格 effort 映射（1=none..6=max）
	if eff := effortLevel(body); eff > 0 {
		w.msg(13, func(x *pbBuf) { x.varint(1, uint64(eff)) })
	}
	if v, ok := body["parallel_tool_calls"].(bool); ok {
		w.msg(14, func(x *pbBuf) { x.varint(1, boolU64(v)) })
	}
	if tools, ok := body["tools"].([]interface{}); ok {
		for _, ti := range tools {
			if tm, ok := ti.(map[string]interface{}); ok {
				encodeTool(w, 16, tm)
			}
		}
	}
	if tc, ok := body["tool_choice"]; ok && tc != nil {
		w.pbValueField(17, normalizeToolChoice(tc))
	}
	if rf, ok := body["response_format"].(map[string]interface{}); ok {
		w.msg(18, func(x *pbBuf) { encodeResponseFormat(x, rf) })
	}
	// stream_options.include_usage
	if so, ok := body["stream_options"].(map[string]interface{}); ok {
		if inc, ok := so["include_usage"].(bool); ok {
			w.msg(20, func(x *pbBuf) { x.msg(1, func(b *pbBuf) { b.varint(1, boolU64(inc)) }) })
		}
	}
	// metadata.context（field 22 → ChatMetadata.context=3 → ContextMetadata）：
	// request_id=1/session_id=2/request_set_id=4/client_type=6（worker Ygn 同构）。
	if requestID != "" || sessionID != "" {
		w.msg(22, func(md *pbBuf) {
			md.msg(3, func(cx *pbBuf) {
				if requestID != "" {
					cx.str(1, requestID)
				}
				if sessionID != "" {
					cx.str(2, sessionID)
				}
				cx.str(6, clientTypeDesktop)
			})
		})
	}
	return w.b
}

// effortLevel 解析 reasoning_effort / reasoning.effort → proto 枚举。
func effortLevel(body map[string]interface{}) int {
	var raw interface{}
	if r, ok := body["reasoning_effort"]; ok {
		raw = r
	} else if rm, ok := body["reasoning"].(map[string]interface{}); ok {
		raw = rm["effort"]
	}
	s, _ := raw.(string)
	switch s {
	case "none":
		return 1
	case "low":
		return 2
	case "medium":
		return 3
	case "high":
		return 4
	case "xhigh":
		return 5
	case "max":
		return 6
	}
	return 0
}

// normalizeToolChoice 规范 tool_choice 为 Value 可编码形态。
func normalizeToolChoice(tc interface{}) interface{} {
	switch v := tc.(type) {
	case string:
		return v
	case map[string]interface{}:
		return v
	default:
		return "auto"
	}
}

// encodeChatMessage 编码单条 ChatMessage（field 为消息在请求里的字段号 2）。
func encodeChatMessage(w *pbBuf, field int, m map[string]interface{}) {
	w.msg(field, func(x *pbBuf) {
		if s := fmt.Sprint(m["role"]); s != "" && s != "<nil>" {
			x.str(2, s)
		}
		switch c := m["content"].(type) {
		case string:
			if c != "" {
				x.str(3, c)
			}
		case []interface{}:
			// 多模态 parts → parts_content(12)/ContentPartList(1)
			x.msg(12, func(pl *pbBuf) {
				for _, pi := range c {
					if pm, ok := pi.(map[string]interface{}); ok {
						pl.msg(1, func(cp *pbBuf) {
							cp.str(1, fmt.Sprint(pm["type"]))
							if txt, ok := pm["text"].(string); ok && txt != "" {
								cp.msg(2, func(sv *pbBuf) { sv.str(1, txt) })
							}
							if iu, ok := pm["image_url"].(map[string]interface{}); ok {
								cp.msg(3, func(iu2 *pbBuf) {
									iu2.str(1, fmt.Sprint(iu["url"]))
									if d, ok := iu["detail"].(string); ok && d != "" {
										iu2.msg(2, func(sv *pbBuf) { sv.str(1, d) })
									}
								})
							}
						})
					}
				}
			})
		}
		if n, ok := m["name"].(string); ok && n != "" {
			x.msg(4, func(sv *pbBuf) { sv.str(1, n) })
		}
		if tcs, ok := m["tool_calls"].([]interface{}); ok {
			for _, tci := range tcs {
				if tcm, ok := tci.(map[string]interface{}); ok {
					encodeToolCall(x, 5, tcm)
				}
			}
		}
		if tid, ok := m["tool_call_id"].(string); ok && tid != "" {
			x.msg(6, func(sv *pbBuf) { sv.str(1, tid) })
		}
		if rc, ok := m["reasoning_content"].(string); ok && rc != "" {
			x.msg(8, func(sv *pbBuf) { sv.str(1, rc) })
		}
	})
}

// encodeToolCall 编码 ToolCall{id=1,type=3,function=4}。
func encodeToolCall(x *pbBuf, field int, tcm map[string]interface{}) {
	x.msg(field, func(tc *pbBuf) {
		if id, ok := tcm["id"].(string); ok && id != "" {
			tc.msg(1, func(sv *pbBuf) { sv.str(1, id) })
		}
		if ty, ok := tcm["type"].(string); ok && ty != "" {
			tc.msg(3, func(sv *pbBuf) { sv.str(1, ty) })
		}
		if fn, ok := tcm["function"].(map[string]interface{}); ok {
			tc.msg(4, func(fc *pbBuf) {
				fc.str(1, fmt.Sprint(fn["name"]))
				// arguments: JSON 字符串 → Value.string_value；非字符串 → 通用 Value
				if args, ok := fn["arguments"].(string); ok {
					fc.msg(2, func(v *pbBuf) { v.str(3, args) })
				} else if raw, exists := fn["arguments"]; exists && raw != nil {
					fc.pbValueField(2, raw)
				}
			})
		}
	})
}

// encodeTool 编码 Tool{type=2,function=3}。
func encodeTool(w *pbBuf, field int, tm map[string]interface{}) {
	w.msg(field, func(t *pbBuf) {
		if ty, ok := tm["type"].(string); ok && ty != "" {
			t.str(2, ty)
		}
		if fn, ok := tm["function"].(map[string]interface{}); ok {
			t.msg(3, func(f *pbBuf) {
				f.str(1, fmt.Sprint(fn["name"]))
				if d, ok := fn["description"].(string); ok && d != "" {
					f.msg(2, func(sv *pbBuf) { sv.str(1, d) })
				}
				if params, ok := fn["parameters"].(map[string]interface{}); ok {
					f.pbStructField(3, params)
				}
			})
		}
	})
}

// encodeResponseFormat 编码 ResponseFormat{type=1,json_schema=2}。
func encodeResponseFormat(x *pbBuf, rf map[string]interface{}) {
	if ty, ok := rf["type"].(string); ok {
		x.str(1, ty)
	}
	if js, ok := rf["json_schema"].(map[string]interface{}); ok {
		x.msg(2, func(j *pbBuf) {
			if n, ok := js["name"].(string); ok {
				j.str(1, n)
			}
			if d, ok := js["description"].(string); ok && d != "" {
				j.msg(2, func(sv *pbBuf) { sv.str(1, d) })
			}
			if sc, ok := js["schema"].(map[string]interface{}); ok {
				j.pbStructField(3, sc)
			}
		})
	}
}

// ---- 响应解码 ----

// decodeChunk 把 ChatCompletionChunk 解码为 OpenAI 兼容 chunk（choices 与 usage）。
func decodeChunk(b []byte) map[string]interface{} {
	id := pbString(b, 2)
	created := int64(pbVarint(b, 4))
	model := pbString(b, 5)
	choices := []interface{}{}
	for _, cb := range pbMessages(b, 6) {
		idx := int32(pbVarint(cb, 1))
		finish := pbString(cb, 3)
		delta := map[string]interface{}{}
		if db := pbMessage(cb, 2); db != nil {
			if r := pbString(db, 1); r != "" {
				delta["role"] = r
			}
			if c := pbString(db, 2); c != "" {
				delta["content"] = c
			}
			if rc := pbString(db, 3); rc != "" {
				delta["reasoning_content"] = rc
			}
			if tcs := decodeToolCallStructs(db); len(tcs) > 0 {
				delta["tool_calls"] = tcs
			}
		}
		var fr interface{}
		if finish != "" {
			fr = finish
		}
		choices = append(choices, map[string]interface{}{
			"index":         idx,
			"delta":         delta,
			"finish_reason": fr,
		})
	}
	out := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": choices,
	}
	if ub := pbMessage(b, 7); ub != nil {
		usage := map[string]interface{}{
			"prompt_tokens":     int32(pbVarint(ub, 1)),
			"completion_tokens": int32(pbVarint(ub, 2)),
			"total_tokens":      int32(pbVarint(ub, 3)),
		}
		out["usage"] = usage
	}
	if code := pbVarint(b, 1); code != 0 {
		out["code"] = code
	}
	return out
}

// decodeToolCallStructs 解析 ResponseChatMessage.tool_calls（repeated Struct）。
// 每个 Struct 形如 {id,type,function:{name,arguments(string_value JSON)}}；
// arguments 是 Value.string_value（JSON 文本）或 struct_value。
func decodeToolCallStructs(db []byte) []interface{} {
	var out []interface{}
	idx := 0
	for _, tb := range pbMessages(db, 4) {
		fields := decodeStruct(tb)
		if fields == nil {
			continue
		}
		call := map[string]interface{}{
			"index": idx,
		}
		if v, ok := fields["id"].(string); ok {
			call["id"] = v
		}
		if v, ok := fields["type"].(string); ok {
			call["type"] = v
		} else {
			call["type"] = "function"
		}
		fn := map[string]interface{}{}
		if fm, ok := fields["function"].(map[string]interface{}); ok {
			if n, ok := fm["name"].(string); ok {
				fn["name"] = n
			}
			switch a := fm["arguments"].(type) {
			case string:
				fn["arguments"] = a
			case map[string]interface{}:
				fn["arguments"] = mustJSONString(a)
			}
		}
		call["function"] = fn
		out = append(out, call)
		idx++
	}
	return out
}

// decodeStruct 解码 google.protobuf.Struct → map。
func decodeStruct(b []byte) map[string]interface{} {
	out := map[string]interface{}{}
	it := &pbIter{b: b}
	for {
		f, wire, _, p, ok := it.next()
		if !ok {
			return out
		}
		if f != 1 || wire != 2 {
			continue
		}
		// map entry: key=1 string, value=2 Value
		key := pbString(p, 1)
		vb := pbMessage(p, 2)
		if key != "" && vb != nil {
			out[key] = decodeValue(vb)
		}
	}
}

// decodeValue 解码 google.protobuf.Value。
func decodeValue(b []byte) interface{} {
	it := &pbIter{b: b}
	for {
		f, wire, num, p, ok := it.next()
		if !ok {
			return nil
		}
		switch {
		case f == 1 && wire == 0: // null_value
			return nil
		case f == 2 && wire == 1: // number_value
			return math.Float64frombits(num)
		case f == 3 && wire == 2: // string_value
			return string(p)
		case f == 4 && wire == 0: // bool_value
			return num != 0
		case f == 5 && wire == 2: // struct_value
			return decodeStruct(p)
		case f == 6 && wire == 2: // list_value
			var list []interface{}
			for _, vb := range pbMessages(p, 1) {
				list = append(list, decodeValue(vb))
			}
			return list
		}
	}
}

// pbChunkCode 提取 chunk 的 code 字段（非 0 = 业务错误）。
func pbChunkCode(b []byte) int64 { return int64(pbVarint(b, 1)) }

// mustJSONString 紧凑序列化（失败退化为 fmt）。
func mustJSONString(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// u32BE 等 helpers 不在 pbwire（避免混用），这里放 gRPC 帧用的大端序。
func u32BE(b []byte, off int) uint32 {
	return binary.BigEndian.Uint32(b[off : off+4])
}

// itoa 快速整数转字符串（错误信息用）。
func itoa(v int) string { return strconv.Itoa(v) }
