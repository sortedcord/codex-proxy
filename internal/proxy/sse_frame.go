package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// Both response modes use the same framing and payload rules. Keep the original
// bytes for passthrough; only strip the optional stream-leading BOM for parsing.
type sseFrame struct {
	raw  []byte
	data []byte
}

type sseScanner struct {
	scanner *bufio.Scanner
	first   bool
}

func newSSEScanner(reader io.Reader) *sseScanner {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxEventBytes+1)
	scanner.Split(splitSSEFrame)
	return &sseScanner{scanner: scanner, first: true}
}

func (s *sseScanner) next() (sseFrame, error) {
	if !s.scanner.Scan() {
		if err := s.scanner.Err(); err != nil {
			return sseFrame{}, err
		}
		return sseFrame{}, io.EOF
	}
	raw := s.scanner.Bytes()
	if len(raw) > maxEventBytes {
		return sseFrame{}, errors.New("upstream SSE frame exceeds size limit")
	}
	parsed := raw
	if s.first {
		parsed = bytes.TrimPrefix(parsed, []byte("\xef\xbb\xbf"))
		s.first = false
	}
	data := []byte{}
	for _, line := range bytes.Split(parsed, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		value := bytes.TrimPrefix(line[len("data:"):], []byte(" "))
		data = append(data, value...)
		data = append(data, '\n')
	}
	return sseFrame{raw: raw, data: bytes.TrimSuffix(data, []byte("\n"))}, nil
}

func splitSSEFrame(data []byte, atEOF bool) (int, []byte, error) {
	var start int
	for start < len(data) {
		index := bytes.IndexByte(data[start:], '\n')
		if index < 0 {
			break
		}
		end := start + index
		if len(bytes.TrimSuffix(data[start:end], []byte("\r"))) == 0 {
			return end + 1, data[:end+1], nil
		}
		start = end + 1
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
