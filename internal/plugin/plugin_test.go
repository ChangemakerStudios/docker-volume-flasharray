package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type stubDriver struct{ created []string }

func (s *stubDriver) Create(r *CreateRequest) error {
	if r.Name == "bad" {
		return errors.New("boom")
	}
	s.created = append(s.created, r.Name+":"+r.Options["size"])
	return nil
}
func (s *stubDriver) List() (*ListResponse, error) {
	return &ListResponse{Volumes: []*Volume{{Name: "a"}}}, nil
}
func (s *stubDriver) Get(r *GetRequest) (*GetResponse, error) {
	switch r.Name {
	case "missing":
		return nil, fmt.Errorf("volume missing: %w", ErrNoSuchVolume)
	case "bad":
		return nil, errors.New("array unreachable")
	}
	return &GetResponse{Volume: &Volume{Name: r.Name, Mountpoint: "/mnt/x"}}, nil
}
func (s *stubDriver) Remove(*RemoveRequest) error { return nil }
func (s *stubDriver) Path(*PathRequest) (*PathResponse, error) {
	return &PathResponse{Mountpoint: "/mnt/x"}, nil
}
func (s *stubDriver) Mount(r *MountRequest) (*MountResponse, error) {
	return &MountResponse{Mountpoint: "/mnt/" + r.Name}, nil
}
func (s *stubDriver) Unmount(*UnmountRequest) error { return nil }
func (s *stubDriver) Capabilities() *CapabilitiesResponse {
	return &CapabilitiesResponse{Capabilities: Capability{Scope: "global"}}
}

func post(t *testing.T, h http.Handler, path string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: bad json %q: %v", path, rec.Body.String(), err)
	}
	if ct := rec.Header().Get("Content-Type"); ct != contentType {
		t.Errorf("%s: content-type %q", path, ct)
	}
	return rec.Code, out
}

func TestProtocol(t *testing.T) {
	d := &stubDriver{}
	h := NewHandler(d, nil)

	code, out := post(t, h, "/Plugin.Activate", nil)
	if code != 200 || out["Implements"].([]any)[0] != "VolumeDriver" {
		t.Fatalf("activate: %d %v", code, out)
	}

	code, out = post(t, h, "/VolumeDriver.Create", map[string]any{"Name": "v1", "Opts": map[string]string{"size": "1G"}})
	if code != 200 || out["Err"] != nil || d.created[0] != "v1:1G" {
		t.Fatalf("create: %d %v %v", code, out, d.created)
	}

	code, out = post(t, h, "/VolumeDriver.Create", map[string]any{"Name": "bad"})
	if code != 500 || out["Err"] != "boom" {
		t.Fatalf("create error: %d %v", code, out)
	}

	code, out = post(t, h, "/VolumeDriver.Mount", map[string]any{"Name": "v1", "ID": "abc"})
	if code != 200 || out["Mountpoint"] != "/mnt/v1" {
		t.Fatalf("mount: %d %v", code, out)
	}

	_, out = post(t, h, "/VolumeDriver.Get", map[string]any{"Name": "v1"})
	if out["Volume"].(map[string]any)["Mountpoint"] != "/mnt/x" {
		t.Fatalf("get: %v", out)
	}

	_, out = post(t, h, "/VolumeDriver.List", nil)
	if len(out["Volumes"].([]any)) != 1 {
		t.Fatalf("list: %v", out)
	}

	_, out = post(t, h, "/VolumeDriver.Capabilities", nil)
	if out["Capabilities"].(map[string]any)["Scope"] != "global" {
		t.Fatalf("capabilities: %v", out)
	}
}

// Docker asks every plugin about other drivers' volumes, and about an empty
// name at start-up: "not mine" is still an error to Docker but not a warning.
func TestNoSuchVolumeIsNotLoggedAsAFailure(t *testing.T) {
	var buf bytes.Buffer
	h := NewHandler(&stubDriver{}, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	code, out := post(t, h, "/VolumeDriver.Get", map[string]any{"Name": "missing"})
	if code != 500 || out["Err"] != "volume missing: no such volume" {
		t.Fatalf("get missing: %d %v", code, out)
	}
	if buf.Len() != 0 {
		t.Fatalf("a volume this driver does not have was logged as a failure: %s", buf.String())
	}
	if code, _ := post(t, h, "/VolumeDriver.Get", map[string]any{"Name": "bad"}); code != 500 || !strings.Contains(buf.String(), "request failed") {
		t.Fatalf("a real failure was not logged: %d %q", code, buf.String())
	}
}

// Docker gives a starting plugin about 12 s to listen and disables it
// otherwise, so the socket opens before start-up work that can wait on the
// array. A request made meanwhile is answered once Serve runs.
func TestSocketListensBeforeServe(t *testing.T) {
	l, err := listenUnix(t.TempDir(), "p")
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("unix", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("nothing listens before Serve: %v", err)
	}
	_ = c.Close()

	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", l.Addr().String())
	}}}
	got := make(chan string, 1)
	go func() {
		resp, err := client.Post("http://plugin/VolumeDriver.Capabilities", contentType, nil)
		if err != nil {
			got <- err.Error()
			return
		}
		defer resp.Body.Close()
		got <- resp.Status
	}()
	select {
	case s := <-got:
		t.Fatalf("answered before Serve: %s", s)
	case <-time.After(50 * time.Millisecond):
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewHandler(&stubDriver{}, nil).Serve(ctx, l) }()
	select {
	case s := <-got:
		if s != "200 OK" {
			t.Fatalf("queued request: %s", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued request never answered")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}
