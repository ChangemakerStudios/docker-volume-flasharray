// Package plugin implements the Docker volume plugin protocol
// (https://docs.docker.com/engine/extend/plugins_volume/) over a unix socket
// with no dependencies beyond the standard library.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	contentType = "application/vnd.docker.plugins.v1.2+json"
	socketDir   = "/run/docker/plugins"
)

// Driver is what a volume driver implements.
type Driver interface {
	Create(*CreateRequest) error
	List() (*ListResponse, error)
	Get(*GetRequest) (*GetResponse, error)
	Remove(*RemoveRequest) error
	Path(*PathRequest) (*PathResponse, error)
	Mount(*MountRequest) (*MountResponse, error)
	Unmount(*UnmountRequest) error
	Capabilities() *CapabilitiesResponse
}

// Volume is the wire representation of a volume.
type Volume struct {
	Name       string         `json:"Name"`
	Mountpoint string         `json:"Mountpoint,omitempty"`
	CreatedAt  string         `json:"CreatedAt,omitempty"`
	Status     map[string]any `json:"Status,omitempty"`
}

// Request/response types, field names as the protocol spells them.
type (
	CreateRequest struct {
		Name    string            `json:"Name"`
		Options map[string]string `json:"Opts,omitempty"`
	}
	RemoveRequest  struct{ Name string }
	MountRequest   struct{ Name, ID string }
	MountResponse  struct{ Mountpoint string }
	UnmountRequest struct{ Name, ID string }
	PathRequest    struct{ Name string }
	PathResponse   struct{ Mountpoint string }
	GetRequest     struct{ Name string }
	GetResponse    struct{ Volume *Volume }
	ListResponse   struct{ Volumes []*Volume }
	Capability     struct{ Scope string }
	// CapabilitiesResponse tells Docker whether volumes are node-local or global.
	CapabilitiesResponse struct{ Capabilities Capability }
)

// errResponse is what every endpoint returns on failure.
type errResponse struct {
	Err string `json:"Err"`
}

// Handler serves a Driver.
type Handler struct {
	d   Driver
	mux *http.ServeMux
	log *slog.Logger
}

// NewHandler wires the routes.
func NewHandler(d Driver, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	h := &Handler{d: d, mux: http.NewServeMux(), log: log.With("component", "plugin-api")}
	h.handle("/Plugin.Activate", func(_ []byte) (any, error) {
		return map[string][]string{"Implements": {"VolumeDriver"}}, nil
	})
	h.handle("/VolumeDriver.Create", func(b []byte) (any, error) {
		var r CreateRequest
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		return struct{}{}, d.Create(&r)
	})
	h.handle("/VolumeDriver.Remove", func(b []byte) (any, error) {
		var r RemoveRequest
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		return struct{}{}, d.Remove(&r)
	})
	h.handle("/VolumeDriver.Mount", func(b []byte) (any, error) {
		var r MountRequest
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		return d.Mount(&r)
	})
	h.handle("/VolumeDriver.Path", func(b []byte) (any, error) {
		var r PathRequest
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		return d.Path(&r)
	})
	h.handle("/VolumeDriver.Unmount", func(b []byte) (any, error) {
		var r UnmountRequest
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		return struct{}{}, d.Unmount(&r)
	})
	h.handle("/VolumeDriver.Get", func(b []byte) (any, error) {
		var r GetRequest
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		return d.Get(&r)
	})
	h.handle("/VolumeDriver.List", func(_ []byte) (any, error) { return d.List() })
	h.handle("/VolumeDriver.Capabilities", func(_ []byte) (any, error) { return d.Capabilities(), nil })
	return h
}

func (h *Handler) handle(path string, fn func(body []byte) (any, error)) {
	h.mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var body []byte
		if r.Body != nil {
			body, _ = readAll(r.Body, 1<<20)
		}
		start := time.Now()
		resp, err := fn(body)
		w.Header().Set("Content-Type", contentType)
		if err != nil {
			h.log.Warn("request failed", "path", path, "err", err, "took", time.Since(start).Round(time.Millisecond))
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(errResponse{Err: err.Error()})
			return
		}
		h.log.Debug("request ok", "path", path, "took", time.Since(start).Round(time.Millisecond))
		_ = json.NewEncoder(w).Encode(resp)
	})
}

// ServeHTTP lets the handler be used in tests with httptest.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// ServeUnix listens on /run/docker/plugins/<name>.sock until ctx is cancelled.
func (h *Handler) ServeUnix(ctx context.Context, name string) error {
	if err := os.MkdirAll(socketDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(socketDir, name+".sock")
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	err = srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func readAll(r interface{ Read([]byte) (int, error) }, limit int64) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for int64(len(buf)) < limit {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf, nil
}
