package main

import (
	"encoding/binary"
	"fmt"
	"math"
)

const maxConnectFrame = 64 << 20

func tag(b []byte, field int, wire byte) []byte { return varint(b, uint64(field<<3)|uint64(wire)) }
func varint(b []byte, value uint64) []byte {
	for value >= 0x80 {
		b = append(b, byte(value)|0x80)
		value >>= 7
	}
	return append(b, byte(value))
}
func bytesField(b []byte, field int, value []byte) []byte {
	b = tag(b, field, 2)
	b = varint(b, uint64(len(value)))
	return append(b, value...)
}
func stringField(b []byte, field int, value string) []byte {
	return bytesField(b, field, []byte(value))
}
func uintField(b []byte, field int, value uint64) []byte { return varint(tag(b, field, 0), value) }
func doubleField(b []byte, field int, value float64) []byte {
	b = tag(b, field, 1)
	var raw [8]byte
	binary.LittleEndian.PutUint64(raw[:], math.Float64bits(value))
	return append(b, raw[:]...)
}

func encodeRequest(r devinRequest) []byte {
	var meta []byte
	for _, f := range []struct {
		n int
		s string
	}{{1, r.ideName}, {2, r.version}, {3, r.token}, {4, "en"}, {5, "darwin"}, {7, r.version}, {12, "chisel"}, {28, "chisel"}, {31, r.attestation}} {
		meta = stringField(meta, f.n, f.s)
	}
	var out []byte
	out = bytesField(out, 1, meta)
	out = stringField(out, 2, r.system)
	for _, m := range r.messages {
		var p []byte
		p = stringField(p, 1, m.id)
		p = uintField(p, 2, uint64(m.source))
		p = stringField(p, 3, m.content)
		for _, tc := range m.toolCalls {
			p = bytesField(p, 6, encodeToolCall(tc))
		}
		if m.toolCallID != "" {
			p = stringField(p, 7, m.toolCallID)
		}
		out = bytesField(out, 3, p)
	}
	out = uintField(out, 7, 5)
	var cfg []byte
	cfg = uintField(cfg, 1, 1)
	cfg = uintField(cfg, 2, uint64(r.maxTokens))
	cfg = uintField(cfg, 3, 400)
	cfg = doubleField(cfg, 5, r.temperature)
	cfg = uintField(cfg, 7, 40)
	cfg = doubleField(cfg, 8, r.topP)
	out = bytesField(out, 8, cfg)
	for _, tool := range r.tools {
		var t []byte
		t = stringField(t, 1, tool.name)
		t = stringField(t, 2, tool.description)
		t = stringField(t, 3, tool.schema)
		out = bytesField(out, 10, t)
	}
	out = stringField(out, 16, r.cascadeID)
	out = uintField(out, 20, 1)
	out = stringField(out, 21, r.model)
	return out
}
func encodeToolCall(tc toolCall) []byte {
	var b []byte
	b = stringField(b, 1, tc.ID)
	b = stringField(b, 2, tc.Name)
	return stringField(b, 3, tc.Arguments)
}
func connectFrame(payload []byte) []byte {
	b := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(b[1:5], uint32(len(payload)))
	copy(b[5:], payload)
	return b
}
func nextConnectFrame(buf []byte) (flag byte, payload, remaining []byte, complete bool, err error) {
	if len(buf) < 5 {
		return 0, nil, buf, false, nil
	}
	n := uint64(binary.BigEndian.Uint32(buf[1:5]))
	if n > maxConnectFrame {
		return 0, nil, nil, false, fmt.Errorf("connect frame exceeds %d bytes", maxConnectFrame)
	}
	if uint64(len(buf)-5) < n {
		return 0, nil, buf, false, nil
	}
	return buf[0], buf[5 : 5+int(n)], buf[5+int(n):], true, nil
}

type protoField struct {
	number  int
	wire    byte
	bytes   []byte
	integer uint64
}

func consumeField(data []byte) (protoField, []byte, error) {
	v, n, err := consumeVarint(data)
	if err != nil {
		return protoField{}, nil, err
	}
	data = data[n:]
	f := protoField{number: int(v >> 3), wire: byte(v & 7)}
	switch f.wire {
	case 0:
		f.integer, n, err = consumeVarint(data)
		if err != nil {
			return f, nil, err
		}
		data = data[n:]
	case 1:
		if len(data) < 8 {
			return f, nil, fmt.Errorf("truncated fixed64")
		}
		data = data[8:]
	case 2:
		var l uint64
		l, n, err = consumeVarint(data)
		if err != nil {
			return f, nil, err
		}
		data = data[n:]
		if l > uint64(len(data)) {
			return f, nil, fmt.Errorf("truncated bytes field")
		}
		f.bytes = data[:int(l)]
		data = data[int(l):]
	case 5:
		if len(data) < 4 {
			return f, nil, fmt.Errorf("truncated fixed32")
		}
		data = data[4:]
	default:
		return f, nil, fmt.Errorf("unsupported protobuf wire type %d", f.wire)
	}
	return f, data, nil
}
func consumeVarint(data []byte) (uint64, int, error) {
	var v uint64
	for i, b := range data {
		if i == 10 || i == 9 && b > 1 {
			return 0, 0, fmt.Errorf("invalid varint")
		}
		v |= uint64(b&0x7f) << uint(7*i)
		if b < 0x80 {
			return v, i + 1, nil
		}
	}
	return 0, 0, fmt.Errorf("truncated varint")
}

func parseResponse(data []byte) (c responseChunk, err error) {
	for len(data) > 0 {
		var f protoField
		f, data, err = consumeField(data)
		if err != nil {
			return c, err
		}
		switch f.number {
		case 1:
			c.ID = string(f.bytes)
		case 3:
			c.Text = string(f.bytes)
		case 5:
			c.Stop = int(f.integer)
		case 6:
			tc, e := parseToolCall(f.bytes)
			if e != nil {
				return c, e
			}
			c.ToolCalls = append(c.ToolCalls, tc)
		case 7:
			c.Usage, err = parseUsage(f.bytes)
		case 9:
			c.Reasoning = string(f.bytes)
		}
	}
	return c, err
}
func parseToolCall(data []byte) (tc toolCall, err error) {
	for len(data) > 0 {
		var f protoField
		f, data, err = consumeField(data)
		if err != nil {
			return
		}
		switch f.number {
		case 1:
			tc.ID = string(f.bytes)
		case 2:
			tc.Name = string(f.bytes)
		case 3:
			tc.Arguments = string(f.bytes)
		}
	}
	return
}
func parseUsage(data []byte) (u *usageStats, err error) {
	u = &usageStats{}
	for len(data) > 0 {
		var f protoField
		f, data, err = consumeField(data)
		if err != nil {
			return
		}
		switch f.number {
		case 2:
			u.Input = f.integer
		case 3:
			u.Output = f.integer
		case 4:
			u.CacheWrite = f.integer
		case 5:
			u.CacheRead = f.integer
		}
	}
	return
}
