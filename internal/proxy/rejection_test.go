package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestInferenceEarlyRejectionsDoNotDrainBodies(t *testing.T) {
	cases := []struct {
		name     string
		method   string
		path     string
		headers  string
		noAuth   bool
		noCreds  bool
		expected int
	}{
		{name: "unauthorized", method: "POST", path: "/v1/responses", noAuth: true, expected: 401},
		{name: "method", method: "POST", path: "/v1/models", expected: 405},
		{name: "route", method: "POST", path: "/unknown", expected: 404},
		{name: "credentials", method: "POST", path: "/v1/responses", noCreds: true, expected: 503},
		{
			name: "content-type", method: "POST", path: "/v1/responses",
			headers: "Content-Type: text/html\r\n", expected: 415,
		},
		{
			name: "encoding", method: "POST", path: "/v1/responses",
			headers: "Content-Encoding: gzip\r\n", expected: 415,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.NotFoundHandler())
			defer upstream.Close()
			p := testProxy(t, upstream)
			p.cfg.APIKey = "key"
			if test.noCreds {
				if err := os.Remove(p.cfg.AuthFile); err != nil {
					t.Fatal(err)
				}
			}
			server := httptest.NewUnstartedServer(p)
			server.Config.ReadTimeout = 5 * time.Second
			server.Start()
			defer server.Close()
			conn, err := net.Dial("tcp", server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			headers := test.headers
			if !test.noAuth {
				headers += "Authorization: Bearer key\r\n"
			}
			_, err = fmt.Fprintf(
				conn,
				"%s %s HTTP/1.1\r\nHost: local\r\nContent-Length: 1\r\n%s\r\n",
				test.method,
				test.path,
				headers,
			)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("rejection waited for missing body: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != test.expected {
				t.Fatalf("status=%d, want %d", resp.StatusCode, test.expected)
			}
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatalf("error response body was not delivered promptly: %v", err)
			}
		})
	}
}
