package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"time"
)

type contextKey string

const requestIDKey contextKey = "requestID"

// APIError defines the standard unified error response model.
type APIError struct {
	Error   string      `json:"error"`
	Code    string      `json:"code"`
	Details interface{} `json:"details,omitempty"`
}

// writeJSON writes a JSON payload with UTF-8 content type and the given HTTP status code.
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if data != nil {
		_ = json.NewEncoder(w).Encode(data)
	}
}

// writeError writes a standardized APIError payload.
func writeError(w http.ResponseWriter, status int, code, msg string, details interface{}) {
	if details == nil {
		details = struct{}{}
	}
	writeJSON(w, status, APIError{
		Error:   msg,
		Code:    code,
		Details: details,
	})
}

// statusRecorder captures HTTP response status and written bytes for request tracing.
type statusRecorder struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int64
}

func (r *statusRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytesWritten += int64(n)
	return n, err
}

// recoveryMiddleware catches unexpected panics and writes a 500 PANIC_RECOVERED response.
func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				stack := string(debug.Stack())
				log.Printf("[PANIC RECOVERED] %v\n%s", rec, stack)
				writeError(w, http.StatusInternalServerError, "PANIC_RECOVERED",
					"internal server error", map[string]interface{}{"panic": fmt.Sprint(rec)})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// tracingMiddleware assigns or preserves X-Request-ID and logs request metrics.
func (s *Server) tracingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			reqID = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-ID", reqID)
		ctx := context.WithValue(r.Context(), requestIDKey, reqID)

		rec := &statusRecorder{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}

		start := time.Now()
		next.ServeHTTP(rec, r.WithContext(ctx))
		duration := time.Since(start)

		log.Printf("[REQ] %s | %s %s | %d | %v | %s",
			reqID, r.Method, r.URL.Path, rec.statusCode, duration, r.RemoteAddr)
	})
}

// corsMiddleware injects CORS headers and handles pre-flight OPTIONS requests.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS, PUT")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID")
		w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireAdmin enforces the AllowAdminActions guard on mutating operations.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.AllowAdminActions {
			writeError(w, http.StatusForbidden, "ADMIN_ACTIONS_DISABLED",
				"admin actions are disabled by configuration", nil)
			return
		}
		next(w, r)
	}
}
