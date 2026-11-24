package localapi

import (
	"net/http"
	"slices"
	"strings"
)

var (
	// connectrpc.com/cors request headers, plus the bearer credential.
	corsRequestHeaders = strings.Join([]string{
		"Authorization", "Content-Type", "Connect-Protocol-Version", "Connect-Timeout-Ms",
		"Grpc-Timeout", "X-Grpc-Web", "X-User-Agent",
	}, ", ")
	// connectrpc.com/cors exposes the gRPC-Web status headers.
	corsExposedHeaders = strings.Join([]string{
		"Grpc-Status", "Grpc-Message", "Grpc-Status-Details-Bin",
	}, ", ")
)

func browserGuard(hosts, origins []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowedHost := slices.ContainsFunc(hosts, func(host string) bool { return strings.EqualFold(host, r.Host) })
		if !allowedHost {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		origin := r.Header.Get("Origin")
		_, hasOrigin := r.Header["Origin"]

		if hasOrigin && (origin == "null" || !slices.Contains(origins, origin)) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		if r.Method == http.MethodOptions && strings.HasPrefix(r.URL.Path, "/") &&
			hasOrigin && r.Header.Get("Access-Control-Request-Method") != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "POST")
			w.Header().Set("Access-Control-Allow-Headers", corsRequestHeaders)
			w.Header().Set("Access-Control-Max-Age", "7200")
			w.Header().Set("Vary", "Origin, Access-Control-Request-Method, Access-Control-Request-Headers")
			w.WriteHeader(http.StatusNoContent)

			return
		}

		if hasOrigin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Expose-Headers", corsExposedHeaders)
			w.Header().Set("Vary", "Origin")
		}

		next.ServeHTTP(w, r)
	})
}
