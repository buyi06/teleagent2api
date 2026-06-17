package middleware

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

func Auth(expectedKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if expectedKey == "" {
				slog.WarnContext(r.Context(), "auth: no API_KEY configured, allowing all requests",
					slog.String("remote", r.RemoteAddr),
				)
				next.ServeHTTP(w, r)
				return
			}

			authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
			got := []byte(authHeader)
			want := []byte("Bearer " + expectedKey)
			if subtle.ConstantTimeCompare(got, want) == 1 {
				next.ServeHTTP(w, r)
				return
			}

			slog.WarnContext(r.Context(), "auth: rejected",
				slog.String("remote", r.RemoteAddr),
				slog.String("path", r.URL.Path),
			)

			w.Header().Set("WWW-Authenticate", "Bearer")
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid api key"})
		})
	}
}