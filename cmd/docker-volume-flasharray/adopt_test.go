package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/flasharray"
)

// tiny in-memory Array for the adopt planner.
type memArray struct {
	vols map[string]*flasharray.Volume
	tags map[string]string
}

func (m *memArray) GetVolume(_ context.Context, n string) (*flasharray.Volume, error) {
	if v, ok := m.vols[n]; ok {
		return v, nil
	}
	return nil, flasharray.ErrNotFound
}
func (m *memArray) CreateVolume(context.Context, string, int64) (*flasharray.Volume, error) {
	panic("not used")
}
func (m *memArray) DestroyVolume(context.Context, string, bool) error { panic("not used") }
func (m *memArray) CopyVolume(context.Context, string, string) (*flasharray.Volume, error) {
	panic("not used")
}
func (m *memArray) SetTag(context.Context, string, string, string) error { panic("not used") }
func (m *memArray) GetTags(context.Context, string) (map[string]string, error) {
	panic("not used")
}
func (m *memArray) DeleteTag(context.Context, string, string) error { panic("not used") }
func (m *memArray) SetNameTag(_ context.Context, v, val string) error {
	m.tags[v] = val
	return nil
}
func (m *memArray) ListNameTags(_ context.Context, p string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range m.tags {
		if strings.HasPrefix(v, p) {
			out[k] = v
		}
	}
	return out, nil
}
func (m *memArray) LookupNameTag(_ context.Context, val string) (string, error) {
	for k, v := range m.tags {
		if v == val {
			return k, nil
		}
	}
	return "", flasharray.ErrNotFound
}
func (m *memArray) ListVolumes(_ context.Context, p string) ([]*flasharray.Volume, error) {
	var out []*flasharray.Volume
	for n, v := range m.vols {
		if strings.HasPrefix(n, p) && !v.Destroyed {
			out = append(out, v)
		}
	}
	return out, nil
}
func (m *memArray) EnsureHost(context.Context, string, []string, []string) (string, error) {
	return "h", nil
}
func (m *memArray) Connect(context.Context, string, string) (int, error) { return 0, nil }
func (m *memArray) Disconnect(context.Context, string, string) error     { return nil }
func (m *memArray) ListConnections(context.Context, string) ([]flasharray.Connection, error) {
	return nil, nil
}
func (m *memArray) Ports(context.Context) ([]flasharray.Port, error) { return nil, nil }

func TestAdoptPrefix(t *testing.T) {
	a := &memArray{
		vols: map[string]*flasharray.Volume{
			"prod-db-1":     {Name: "prod-db-1", Serial: "a", Created: time.Now()},
			"prod-app_data": {Name: "prod-app_data", Serial: "b", Created: time.Now()},
			"prod-old":           {Name: "prod-old", Serial: "c", Destroyed: true},
			"unrelated":                {Name: "unrelated", Serial: "d"},
		},
		tags: map[string]string{"prod-db-1": "docker/db-1"}, // already adopted
	}
	var out bytes.Buffer
	if err := adopt(context.Background(), a, "docker", "prod-", nil, true, &out, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would   prod-app_data") || !strings.Contains(out.String(), "skip    prod-db-1") {
		t.Fatalf("dry-run output:\n%s", out.String())
	}
	if len(a.tags) != 1 {
		t.Fatal("dry-run must not write tags")
	}
	out.Reset()
	if err := adopt(context.Background(), a, "docker", "prod-", []string{"unrelated=weird_name"}, false, &out, nil); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if a.tags["prod-app_data"] != "docker/app_data" || a.tags["unrelated"] != "docker/weird_name" {
		t.Fatalf("tags = %v", a.tags)
	}
	if _, ok := a.tags["prod-old"]; ok {
		t.Fatal("destroyed volume must be skipped")
	}
	// conflict: same docker name, different array volume
	out.Reset()
	err := adopt(context.Background(), a, "docker", "", []string{"unrelated=app_data"}, false, &out, nil)
	if err == nil || !strings.Contains(out.String(), "CONFLICT") && !strings.Contains(out.String(), "FAILED") {
		t.Fatalf("expected conflict, got %v\n%s", err, out.String())
	}
}

func TestAdoptDryRunReportsNameConflict(t *testing.T) {
	a := &memArray{
		vols: map[string]*flasharray.Volume{
			"pre-a":   {Name: "pre-a", Serial: "a"},
			"pre-b":   {Name: "pre-b", Serial: "b"},
			"current": {Name: "current", Serial: "c"},
		},
		tags: map[string]string{"current": "docker/a"},
	}
	var out bytes.Buffer
	// pre-a collides with an existing mapping; pre-b and the explicit entry collide with each other.
	err := adopt(context.Background(), a, "docker", "pre-", []string{"pre-a=b"}, true, &out, nil)
	if err == nil {
		t.Fatalf("expected dry-run to fail\n%s", out.String())
	}
	if got := strings.Count(out.String(), "CONFLICT"); got != 2 {
		t.Fatalf("want 2 conflicts, got %d\n%s", got, out.String())
	}
	if len(a.tags) != 1 {
		t.Fatal("dry-run must not write tags")
	}
}
