package proxy

import (
	"strings"
	"testing"
)

func BenchmarkPrepareRequest(b *testing.B) {
	body := []byte(`{
		"model":"gpt-4o",
		"input":[{"role":"user","content":"test benchmark input"}],
		"stream":false
	}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = prepareRequest(body, false)
	}
}

func BenchmarkCollectResponse(b *testing.B) {
	stream := strings.Join([]string{
		": keepalive\n\n",
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1"}}` + "\n\n",
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}` + "\n\n",
	}, "")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = collectResponse(strings.NewReader(stream))
	}
}
