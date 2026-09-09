// Derived from CLIProxyAPIPlus commit 1fec8453e63a5bc133555a79164480700e351bfc; MIT licensed.

package main

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
)

const maxEventStreamFrame = 10 << 20

type eventMessage struct {
	EventType     string
	MessageType   string
	ExceptionType string
	Payload       []byte
}

func readEventMessage(r io.Reader) (*eventMessage, error) {
	var prelude [12]byte
	if _, err := io.ReadFull(r, prelude[:]); err == io.EOF {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("read event prelude: %w", err)
	}
	total := binary.BigEndian.Uint32(prelude[:4])
	headersLen := binary.BigEndian.Uint32(prelude[4:8])
	if total < 16 || total > maxEventStreamFrame || headersLen > total-16 {
		return nil, fmt.Errorf("invalid event frame lengths total=%d headers=%d", total, headersLen)
	}
	if binary.BigEndian.Uint32(prelude[8:]) != crc32.ChecksumIEEE(prelude[:8]) {
		return nil, fmt.Errorf("event prelude checksum mismatch")
	}
	rest := make([]byte, int(total)-12)
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, fmt.Errorf("read event body: %w", err)
	}
	full := append(append([]byte(nil), prelude[:]...), rest[:len(rest)-4]...)
	if binary.BigEndian.Uint32(rest[len(rest)-4:]) != crc32.ChecksumIEEE(full) {
		return nil, fmt.Errorf("event message checksum mismatch")
	}
	headers, err := eventHeaders(rest[:headersLen])
	if err != nil {
		return nil, err
	}
	payload := append([]byte(nil), rest[headersLen:len(rest)-4]...)
	return &eventMessage{EventType: headers[":event-type"], MessageType: headers[":message-type"], ExceptionType: headers[":exception-type"], Payload: payload}, nil
}

func eventHeaders(data []byte) (map[string]string, error) {
	out := map[string]string{}
	for i := 0; i < len(data); {
		nameLen := int(data[i])
		i++
		if nameLen == 0 || i+nameLen+1 > len(data) {
			return nil, fmt.Errorf("invalid event header name")
		}
		name := string(data[i : i+nameLen])
		i += nameLen
		kind := data[i]
		i++
		switch kind {
		case 0, 1:
			out[name] = map[bool]string{true: "true", false: "false"}[kind == 0]
		case 2:
			i++
		case 3:
			i += 2
		case 4:
			i += 4
		case 5, 8:
			i += 8
		case 9:
			i += 16
		case 6, 7:
			if i+2 > len(data) {
				return nil, fmt.Errorf("invalid event header length")
			}
			n := int(binary.BigEndian.Uint16(data[i : i+2]))
			i += 2
			if i+n > len(data) {
				return nil, fmt.Errorf("event header overflow")
			}
			if kind == 7 {
				out[name] = string(data[i : i+n])
			}
			i += n
		default:
			return nil, fmt.Errorf("unsupported event header type %d", kind)
		}
		if i > len(data) {
			return nil, fmt.Errorf("event header overflow")
		}
	}
	return out, nil
}
