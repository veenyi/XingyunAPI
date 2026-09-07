package qwenwork

// pbwire：protobuf wire format 手写编解码原语（proto3）。
// 只实现 chat.proto 需要的形态：varint（wire 0）、fixed64/double（wire 1）、
// length-delimited（wire 2，string/bytes/message/map-entry）。

import (
	"encoding/binary"
	"math"
)

// pbBuf 是 proto 消息写入器。
type pbBuf struct{ b []byte }

func (w *pbBuf) rawVarint(v uint64) {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	w.b = append(w.b, tmp[:n]...)
}

func (w *pbBuf) tag(field, wire int) { w.rawVarint(uint64(field)<<3 | uint64(wire)) }

// str 写 string 字段（空串仍写入：oneof 成员与显式空串需保留）。
func (w *pbBuf) str(field int, s string) {
	w.tag(field, 2)
	w.rawVarint(uint64(len(s)))
	w.b = append(w.b, s...)
}

// bytesField 写 bytes/message 字段（len(b)==0 时跳过，proto3 空消息省略）。
func (w *pbBuf) bytesField(field int, b []byte) {
	if len(b) == 0 {
		return
	}
	w.tag(field, 2)
	w.rawVarint(uint64(len(b)))
	w.b = append(w.b, b...)
}

// double 写 fixed64 double 字段。
func (w *pbBuf) double(field int, f float64) {
	w.tag(field, 1)
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], math.Float64bits(f))
	w.b = append(w.b, tmp[:]...)
}

// varint 写 varint 字段（proto3 默认值 0 会写入：调用方自行决定是否省略）。
func (w *pbBuf) varint(field int, v uint64) {
	w.tag(field, 0)
	w.rawVarint(v)
}

// msg 写嵌套消息（fn 产出子消息字节，空则跳过）。
func (w *pbBuf) msg(field int, fn func(*pbBuf)) {
	sub := &pbBuf{}
	fn(sub)
	w.bytesField(field, sub.b)
}

// pbIter 是 proto 消息字段迭代器。
type pbIter struct{ b []byte }

// next 返回下一个字段：field/wire；num 为 varint/fixed 值；payload 为 wire2 内容。
func (it *pbIter) next() (field, wire int, num uint64, payload []byte, ok bool) {
	if len(it.b) == 0 {
		return 0, 0, 0, nil, false
	}
	key, n := binary.Uvarint(it.b)
	if n <= 0 {
		return 0, 0, 0, nil, false
	}
	it.b = it.b[n:]
	field, wire = int(key>>3), int(key&7)
	switch wire {
	case 0:
		v, n2 := binary.Uvarint(it.b)
		if n2 <= 0 {
			return 0, 0, 0, nil, false
		}
		it.b = it.b[n2:]
		return field, wire, v, nil, true
	case 1:
		if len(it.b) < 8 {
			return 0, 0, 0, nil, false
		}
		v := binary.LittleEndian.Uint64(it.b[:8])
		it.b = it.b[8:]
		return field, wire, v, nil, true
	case 2:
		l, n2 := binary.Uvarint(it.b)
		if n2 <= 0 || uint64(len(it.b)-n2) < l {
			return 0, 0, 0, nil, false
		}
		payload = it.b[n2 : n2+int(l)]
		it.b = it.b[n2+int(l):]
		return field, wire, 0, payload, true
	case 5:
		if len(it.b) < 4 {
			return 0, 0, 0, nil, false
		}
		v := uint64(binary.LittleEndian.Uint32(it.b[:4]))
		it.b = it.b[4:]
		return field, wire, v, nil, true
	default:
		return 0, 0, 0, nil, false
	}
}

// pbString 迭代取出 field 的所有 string 值（repeated string）。
func pbStringAll(b []byte, field int) []string {
	it := &pbIter{b: b}
	var out []string
	for {
		f, wire, _, p, ok := it.next()
		if !ok {
			return out
		}
		if f == field && wire == 2 {
			out = append(out, string(p))
		}
	}
}

// pbString 取 field 的最后一个 string 值。
func pbString(b []byte, field int) string {
	out := ""
	it := &pbIter{b: b}
	for {
		f, wire, _, p, ok := it.next()
		if !ok {
			return out
		}
		if f == field && wire == 2 {
			out = string(p)
		}
	}
}

// pbVarint 取 field 的最后一个 varint 值。
func pbVarint(b []byte, field int) uint64 {
	out := uint64(0)
	it := &pbIter{b: b}
	for {
		f, wire, v, _, ok := it.next()
		if !ok {
			return out
		}
		if f == field && wire == 0 {
			out = v
		}
	}
}

// pbMessages 取 field 的所有嵌套消息（repeated message）。
func pbMessages(b []byte, field int) [][]byte {
	it := &pbIter{b: b}
	var out [][]byte
	for {
		f, wire, _, p, ok := it.next()
		if !ok {
			return out
		}
		if f == field && wire == 2 {
			out = append(out, p)
		}
	}
}

// pbMessage 取 field 的最后一个嵌套消息。
func pbMessage(b []byte, field int) []byte {
	out := []byte(nil)
	it := &pbIter{b: b}
	for {
		f, wire, _, p, ok := it.next()
		if !ok {
			return out
		}
		if f == field && wire == 2 {
			out = p
		}
	}
}

// pbDouble 取 field 的 fixed64 double 值。
func pbDouble(b []byte, field int) (float64, bool) {
	it := &pbIter{b: b}
	for {
		f, wire, v, _, ok := it.next()
		if !ok {
			return 0, false
		}
		if f == field && wire == 1 {
			return math.Float64frombits(v), true
		}
	}
}
