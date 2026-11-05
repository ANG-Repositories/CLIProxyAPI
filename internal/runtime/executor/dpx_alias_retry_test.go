package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestDPXAliasNoRetryAfterDispatch(t *testing.T) {
	for _, mode := range []string{"before_headers", "partial_tool", "complete_tool", "reverse_error", "raw_name", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			e, req, opts := aliasFixture(t)
			opts.Stream = true
			var calls atomic.Int32
			var credentials syncCredentials
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				credentials.add(r.Header.Get("X-Api-Key"))
				raw, _ := io.ReadAll(r.Body)
				if mode == "before_headers" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						conn.Close()
					}
					return
				}
				name := gjson.GetBytes(raw, "tools.0.name").String()
				key := ""
				gjson.GetBytes(raw, "tools.0.input_schema.properties").ForEach(func(k, v gjson.Result) bool { key = k.String(); return false })
				if mode == "raw_name" {
					name = "unknown_raw_tool" // Raw undeclared calls belong to the client.
				}
				body := aliasSSE(name, key, req.Model)
				if mode == "reverse_error" {
					// A signed alias-shaped tool block is still an inverse error;
					// an unsigned unknown alias instead becomes unknown_tool_N.
					body = bytes.Replace(body, []byte(`"type":"tool_use"`), []byte(`"signature":"invalid","type":"tool_use"`), 1)
				}
				if mode == "partial_tool" {
					body = body[:bytes.Index(body, []byte("event: content_block_stop"))]
				}
				if mode == "complete_tool" || mode == "cancel" {
					body = body[:bytes.Index(body, []byte("event: message_delta"))]
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Length", fmt.Sprint(len(body)+1024))
				w.Write(body)
				w.(http.Flusher).Flush()
				if mode == "cancel" {
					<-r.Context().Done()
					return
				}
			}))
			defer upstream.Close()
			manager := authpkg.NewManager(nil, nil, nil)
			manager.SetConfig(e.cfg)
			manager.SetRetryConfig(0, 0, 1)
			manager.RegisterExecutor(e)
			for i := 0; i < 2; i++ {
				a := offlineAuth(upstream.URL)
				a.ID = fmt.Sprintf("no-retry-%s-%d", mode, i)
				a.Attributes["api_key"] = fmt.Sprintf("offline-key-%d", i)
				registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: req.Model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
				if _, err := manager.Register(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			response, err := manager.ExecuteStream(ctx, []string{"claude"}, req, opts)
			var out []byte
			if err == nil {
				for chunk := range response.Chunks {
					out = append(out, chunk.Payload...)
					if chunk.Err != nil {
						err = chunk.Err
					}
					if mode == "cancel" && bytes.Contains(out, []byte("content_block_stop")) {
						cancel()
					}
				}
			}
			if mode == "raw_name" {
				// The complete raw tool call is released even though the transport
				// ends early; the post-dispatch transport error must not retry it.
				if !bytes.Contains(out, []byte(`"name":"unknown_raw_tool"`)) || !bytes.Contains(out, []byte(`dpx_v1_`)) || !bytes.Contains(out, []byte(`/tmp/Hermes`)) {
					t.Fatalf("raw call not passed through verbatim: %s", out)
				}
			} else if err == nil && !(mode == "cancel" && ctx.Err() != nil) {
				t.Fatal("incomplete/invalid upstream accepted")
			}
			if calls.Load() != 1 || credentials.count() != 1 {
				t.Fatalf("retry or credential switch: calls=%d identities=%d", calls.Load(), credentials.count())
			}
			if (mode == "partial_tool" || mode == "reverse_error") && bytes.Contains(out, []byte(`"type":"tool_use"`)) {
				t.Fatal("unvalidated tool released")
			}
		})
	}
}

type syncCredentials struct{ values atomic.Value }

func (s *syncCredentials) add(value string) {
	old, _ := s.values.Load().([]string)
	s.values.Store(append(old, value))
}
func (s *syncCredentials) count() int {
	values, _ := s.values.Load().([]string)
	unique := map[string]bool{}
	for _, v := range values {
		unique[v] = true
	}
	return len(unique)
}
