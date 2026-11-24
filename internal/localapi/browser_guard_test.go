package localapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBrowserGuard_Order(t *testing.T) {
	actual := http.Header{
		"Access-Control-Allow-Origin":   {"http://localhost:5173"},
		"Access-Control-Expose-Headers": {"Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin"},
		"Vary":                          {"Origin"},
	}
	preflight := http.Header{
		"Access-Control-Allow-Origin":  {"http://localhost:5173"},
		"Access-Control-Allow-Methods": {"POST"},
		"Access-Control-Allow-Headers": {
			"Authorization, Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, Grpc-Timeout, X-Grpc-Web, X-User-Agent",
		},
		"Access-Control-Max-Age": {"7200"},
		"Vary":                   {"Origin, Access-Control-Request-Method, Access-Control-Request-Headers"},
	}
	refused := http.Header{
		"Content-Type":           {"text/plain; charset=utf-8"},
		"X-Content-Type-Options": {"nosniff"},
	}
	for _, tc := range []struct {
		name, host, origin, method, path, requestedMethod string
		authorization                                     bool
		status                                            int
		next                                              bool
		headers                                           http.Header
		body                                              string
	}{
		{
			name: "bad host before token", host: "evil.example:7878", origin: "http://localhost:5173", authorization: true,
			status: 403, headers: refused, body: "forbidden\n",
		},
		{name: "empty host", status: 403, headers: refused, body: "forbidden\n"},
		{name: "case insensitive host", host: "LOCALHOST:7878", status: 299, next: true, headers: http.Header{}},
		{
			name: "bad origin", host: "127.0.0.1:7878", origin: "https://evil.example", status: 403,
			headers: refused, body: "forbidden\n",
		},
		{
			name: "null origin", host: "127.0.0.1:7878", origin: "null", status: 403,
			headers: refused, body: "forbidden\n",
		},
		{
			name: "preflight", host: "127.0.0.1:7878", origin: "http://localhost:5173", method: "OPTIONS",
			path: "/coagent.v1.OutputService/TailOutput", requestedMethod: "POST", status: 204, headers: preflight,
		},
		{
			name: "unknown preflight", host: "127.0.0.1:7878", origin: "http://localhost:5173", method: "OPTIONS",
			path: "/nope.Service/Missing", requestedMethod: "POST", status: 204, headers: preflight,
		},
		{
			name: "bad origin preflight", host: "127.0.0.1:7878", origin: "https://evil.example", method: "OPTIONS",
			requestedMethod: "POST", status: 403, headers: refused, body: "forbidden\n",
		},
		{
			name: "options without requested method", host: "127.0.0.1:7878", origin: "http://localhost:5173",
			method: "OPTIONS", status: 299, next: true, headers: actual,
		},
		{
			name: "general options", host: "127.0.0.1:7878", origin: "http://localhost:5173", method: "OPTIONS",
			path: "*", requestedMethod: "POST", status: 299, next: true, headers: actual,
		},
		{name: "no origin", host: "127.0.0.1:7878", status: 299, next: true, headers: http.Header{}},
		{
			name: "allowed origin", host: "127.0.0.1:7878", origin: "http://localhost:5173",
			status: 299, next: true, headers: actual,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := tc.method
			if method == "" {
				method = http.MethodPost
			}
			req := httptest.NewRequest(method, "/x", nil)
			req.Host = tc.host
			if tc.path != "" {
				req.URL.Path = tc.path
				req.RequestURI = tc.path
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.authorization {
				req.Header.Set("Authorization", "Bearer test")
			}
			if tc.requestedMethod != "" {
				req.Header.Set("Access-Control-Request-Method", tc.requestedMethod)
				req.Header.Set("Access-Control-Request-Headers", "authorization,content-type,connect-protocol-version")
			}
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(299)
			})
			response := httptest.NewRecorder()
			browserGuard([]string{"127.0.0.1:7878", "localhost:7878"}, []string{"http://localhost:5173"}, next).
				ServeHTTP(response, req)
			assert.Equal(t, tc.status, response.Code)
			assert.Equal(t, tc.next, called)
			assert.Equal(t, tc.body, response.Body.String())
			assert.Equal(t, tc.headers, response.Header())
			assert.Empty(t, response.Header().Get("Access-Control-Allow-Credentials"))
		})
	}
}
