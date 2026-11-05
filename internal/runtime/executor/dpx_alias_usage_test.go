package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type aliasUsageSink struct{ records chan usage.Record }

func (s *aliasUsageSink) HandleUsage(_ context.Context, r usage.Record) {
	if r.AuthID == "alias-usage-observer" {
		select {
		case s.records <- r:
		default:
		}
	}
}

func TestDPXAliasActualUsageOnInverseError(t *testing.T) {
	sink := &aliasUsageSink{make(chan usage.Record, 8)}
	usage.RegisterNamedPlugin("offline-alias-observer", sink)
	usage.StartDefault(context.Background())
	for _, signed := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("signed=%t/stream=%t", signed, stream), func(t *testing.T) {
				e, req, opts := aliasFixture(t)
				opts.Stream = stream
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					name := "unknown_raw_tool"
					signature := ""
					if signed {
						name = "dpx_v1_t_unknown"
						signature = `,"signature":"invalid"`
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						body := aliasSSE(name, "bad", req.Model)
						if signed {
							body = bytes.Replace(body, []byte(`"type":"tool_use"`), []byte(`"signature":"invalid","type":"tool_use"`), 1)
						}
						w.Write(body)
					} else {
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, fmt.Sprintf(`{"content":[{"type":"tool_use","name":%q,"input":{}%s}],"usage":{"input_tokens":5,"output_tokens":7}}`, name, signature))
					}
				}))
				auth := offlineAuth(server.URL)
				auth.ID = "alias-usage-observer"
				var err error
				var out []byte
				if stream {
					response, e2 := e.ExecuteStream(context.Background(), auth, req, opts)
					err = e2
					if err == nil {
						for chunk := range response.Chunks {
							out = append(out, chunk.Payload...)
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					response, e2 := e.Execute(context.Background(), auth, req, opts)
					err = e2
					out = response.Payload
				}
				server.Close()
				if signed {
					if err == nil || !strings.Contains(err.Error(), "signed_tool_block") {
						t.Fatalf("signed alias inverse should fail: %v", err)
					}
				} else if err != nil || !bytes.Contains(out, []byte(`"name":"unknown_raw_tool"`)) {
					t.Fatalf("raw name should pass through: %v %s", err, out)
				}
				select {
				case record := <-sink.records:
					if record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 7 {
						t.Fatalf("actual usage lost stream=%t: %+v", stream, record.Detail)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("no actual usage record")
				}
			})
		}
	}
}
