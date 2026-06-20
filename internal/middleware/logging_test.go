package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (f *flushRecorder) Flush() {
	f.flushed = true
}

func TestStatusWriterPreservesFlusher(t *testing.T) {
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	sw := &statusWriter{ResponseWriter: rec, status: http.StatusOK}
	flusher, ok := any(sw).(http.Flusher)
	if !ok {
		t.Fatal("statusWriter must implement http.Flusher")
	}
	flusher.Flush()
	if !rec.flushed {
		t.Fatal("underlying Flush was not called")
	}
}
