package flasharray

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// newTestClient serves the api_version and login endpoints and hands every
// other request to h.
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/api_version":
			_, _ = w.Write([]byte(`{"version":["1.19","2.4","2.26"]}`))
		case "/api/2.26/login":
			w.Header().Set("x-auth-token", "tok")
		default:
			h(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(context.Background(), Options{Endpoint: srv.URL, APIToken: "t", InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	c.retryBase = 0
	return c
}

func TestNegotiatesHighest2x(t *testing.T) {
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) {})
	if c.Version() != "2.26" {
		t.Fatalf("version = %s", c.Version())
	}
}

func TestRetries(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		status  int
		wantN   int32
		wantErr bool
	}{
		{"GET 503 is retried", http.MethodGet, http.StatusServiceUnavailable, maxAttempts, true},
		{"POST 503 is not retried", http.MethodPost, http.StatusServiceUnavailable, 1, true},
		{"POST 429 is retried", http.MethodPost, http.StatusTooManyRequests, maxAttempts, true},
		{"400 is not retried", http.MethodGet, http.StatusBadRequest, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var n atomic.Int32
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				n.Add(1)
				w.WriteHeader(tc.status)
			})
			err := c.do(context.Background(), tc.method, "/volumes", nil, nil, nil)
			if (err != nil) != tc.wantErr || n.Load() != tc.wantN {
				t.Fatalf("err=%v requests=%d, want %d", err, n.Load(), tc.wantN)
			}
		})
	}
}

func TestRetrySucceedsAfterTransientError(t *testing.T) {
	var n atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"name":"v","serial":"ABC"}]}`))
	})
	v, err := c.GetVolume(context.Background(), "v")
	if err != nil || v.Serial != "abc" {
		t.Fatalf("v=%+v err=%v", v, err)
	}
}

func TestReloginOn401(t *testing.T) {
	var n atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	if _, err := c.ListConnections(context.Background(), "v"); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 {
		t.Fatalf("requests = %d, want 2", n.Load())
	}
}
