package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
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
