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

func TestVolumeWrittenFromSpaceVirtual(t *testing.T) {
	cases := map[string]int64{
		`{"items":[{"name":"v","serial":"ABC","space":{"virtual":0}}]}`:    0,
		`{"items":[{"name":"v","serial":"ABC","space":{"virtual":8192}}]}`: 8192,
		`{"items":[{"name":"v","serial":"ABC"}]}`:                          -1,
	}
	for body, want := range cases {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
		v, err := c.GetVolume(context.Background(), "v")
		if err != nil || v.Written != want {
			t.Errorf("%s: written=%d err=%v, want %d", body, v.Written, err, want)
		}
	}
}

func TestNameTagsSkipDestroyedVolumes(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/2.26/volumes/tags":
			_, _ = w.Write([]byte(`{"items":[
				{"key":"name","value":"ns/a","resource":{"name":"old-a"}},
				{"key":"name","value":"ns/a","resource":{"name":"new-a"}}]}`))
		case "/api/2.26/volumes":
			if f := r.URL.Query().Get("filter"); f != "destroyed='true'" {
				t.Errorf("volumes filter = %q", f)
			}
			_, _ = w.Write([]byte(`{"items":[{"name":"old-a","destroyed":true}]}`))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	})
	ctx := context.Background()
	tags, err := c.ListNameTags(ctx, "ns/")
	if err != nil || len(tags) != 1 || tags["new-a"] != "ns/a" {
		t.Fatalf("tags = %v, %v", tags, err)
	}
	if an, err := c.LookupNameTag(ctx, "ns/a"); err != nil || an != "new-a" {
		t.Fatalf("lookup = %q, %v", an, err)
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

// The sweep trusts this map to decide which host paths are stale, so a
// connection whose serial cannot be resolved must fail rather than vanish.
func TestHostLUNs(t *testing.T) {
	missing := false
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/2.26/connections":
			if r.URL.Query().Get("host_names") != "node1" {
				t.Errorf("query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"items":[{"host":{"name":"node1"},"volume":{"name":"a"},"lun":3},{"host":{"name":"node1"},"volume":{"name":"b"},"lun":7}]}`))
		case "/api/2.26/volumes":
			if missing {
				_, _ = w.Write([]byte(`{"items":[{"name":"a","serial":"6FEF9AB1157343EC00000001"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"items":[{"name":"a","serial":"6FEF9AB1157343EC00000001"},{"name":"b","serial":"6FEF9AB1157343EC00000002"}]}`))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	})
	got, err := c.HostLUNs(context.Background(), "node1")
	if err != nil || len(got) != 2 || got[3] != "6fef9ab1157343ec00000001" || got[7] != "6fef9ab1157343ec00000002" {
		t.Fatalf("got %v, %v", got, err)
	}
	missing = true
	if _, err := c.HostLUNs(context.Background(), "node1"); err == nil {
		t.Fatal("expected an error when a connected volume has no serial")
	}
}

// Read as "nothing connected", a not-found error here would make the sweep
// delete every path on the node.
func TestHostLUNsNotFoundIsAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Host does not exist."}]}`))
	})
	if _, err := c.HostLUNs(context.Background(), "node1"); err == nil {
		t.Fatal("expected the not-found error, not an empty LUN map")
	}
}

// During a Purity upgrade a controller or proxy can answer 404, or a page
// saying "not found", for a route that is only briefly unavailable. Reading
// that as "no such volume" makes Docker create an empty local volume.
func TestNotFoundOnlyFromArrayJSON400(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		notFound bool
	}{
		{"array says missing", http.StatusBadRequest, `{"errors":[{"context":"v","message":"Volume does not exist."}]}`, true},
		{"route unavailable", http.StatusNotFound, `Not Found`, false},
		{"proxy page", http.StatusBadRequest, `<html><body>404 not found</body></html>`, false},
		{"404 with array json", http.StatusNotFound, `{"errors":[{"message":"Volume does not exist."}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := c.GetVolume(context.Background(), "v")
			if err == nil {
				t.Fatal("expected an error")
			}
			if IsNotFound(err) != tc.notFound {
				t.Fatalf("IsNotFound = %v, want %v (err %v)", IsNotFound(err), tc.notFound, err)
			}
		})
	}
}
