package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStreamingErrorTerminatesCleanly(t *testing.T) {
	frame := "data: {\"type\":\"error\",\"error\":{\"code\":\"context_length_exceeded\"}}\n\n"
	for _, metadata := range []string{"", `,"metadata":{"tenant":"one"}`} {
		t.Run(metadata, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, frame)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer upstream.Close()
			server := httptest.NewServer(testProxy(t, upstream))
			defer server.Close()
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Post(server.URL+"/v1/responses", "application/json",
				strings.NewReader(`{"model":"test","input":[],"stream":true`+metadata+`}`))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("in-band error caused transport failure: %v", err)
			}
			if string(body) != frame {
				t.Fatalf("error frame changed: got %q, want %q", body, frame)
			}
		})
	}
}

func TestStreamReaderValidatesTerminalState(t *testing.T) {
	for _, frame := range []string{
		"", "data: [DONE]\n\n", "data: not-json\n\n",
		"data: {\"type\":\"response.ping\"}\n\n",
		"data: {\"type\":\"response.completed\",\"response\":null}\n\n",
	} {
		reader := newResponseStreamReader(strings.NewReader(frame), nil)
		if _, err := io.ReadAll(reader); err == nil {
			t.Errorf("invalid stream accepted: %q", frame)
		}
	}
	reader := newResponseStreamReader(strings.NewReader("data: {\"type\":\"response.ping\"}\n\n"), nil)
	if _, err := io.ReadAll(reader); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("premature EOF = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestSharedSSEFraming(t *testing.T) {
	for _, prefix := range []string{"", "\ufeff"} {
		frame := prefix + ": comment\r\nevent: response.completed\r\nid: end\r\n" +
			"data: {\"type\":\"response.completed\",\r\n" +
			"data: \"response\":{\"output\":[]}}\r\n\r\n"
		body, err := collectResponse(strings.NewReader(frame))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"output":[]}` {
			t.Fatalf("collected response = %q", body)
		}
		stream, err := io.ReadAll(newResponseStreamReader(strings.NewReader(frame), nil))
		if err != nil {
			t.Fatal(err)
		}
		if string(stream) != frame {
			t.Fatalf("native framing changed: got %q, want %q", stream, frame)
		}
	}
}
