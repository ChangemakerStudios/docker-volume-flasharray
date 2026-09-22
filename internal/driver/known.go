package driver

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"

	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/plugin"
)

// Docker treats any error from a driver's Get as "not found", and for a volume
// declared without a driver (`external: true`) it then silently creates a
// `local` volume of the same name, which shadows the real one on that node
// until someone prunes it. An array management outage (controller failover,
// Purity upgrade) would do exactly that. The known-volume file lets Get keep
// answering "exists" while the array is unreachable.
//
// It is only ever a fallback for Get and List: resolution always goes to the
// array. It is kept under MountRoot (the propagated mount) so it survives
// plugin restarts, and List replaces it with the array's view each time.
const knownFile = ".dvfa-known.json"

func (d *Driver) knownPath() string { return filepath.Join(d.cfg.MountRoot, knownFile) }

func (d *Driver) loadKnown() {
	b, err := os.ReadFile(d.knownPath())
	if err != nil {
		if !os.IsNotExist(err) {
			d.log.Warn("cannot read known-volume file; outage fallback starts empty", "path", d.knownPath(), "err", err)
		}
		return
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		d.log.Warn("known-volume file unreadable; outage fallback starts empty", "path", d.knownPath(), "err", err)
		return
	}
	d.mu.Lock()
	d.known = m
	d.mu.Unlock()
	d.log.Info("loaded known volumes for outage fallback", "volumes", len(m))
}

// setKnown updates one entry (arrayName "" deletes it) and persists on change.
func (d *Driver) setKnown(dockerName, arrayName string) {
	d.mu.Lock()
	cur, ok := d.known[dockerName]
	switch {
	case arrayName == "" && !ok, arrayName != "" && ok && cur == arrayName:
		d.mu.Unlock()
		return
	case arrayName == "":
		delete(d.known, dockerName)
	default:
		d.known[dockerName] = arrayName
	}
	d.mu.Unlock()
	d.saveKnown()
}

// replaceKnown makes the file match the array's full list of tagged volumes.
func (d *Driver) replaceKnown(m map[string]string) {
	d.mu.Lock()
	same := maps.Equal(d.known, m)
	if !same {
		d.known = m
	}
	d.mu.Unlock()
	if !same {
		d.saveKnown()
	}
}

// saveKnown writes atomically. A failure only costs the outage fallback.
func (d *Driver) saveKnown() {
	d.saveMu.Lock()
	defer d.saveMu.Unlock()
	d.mu.Lock()
	b, err := json.Marshal(d.known)
	d.mu.Unlock()
	if err == nil {
		tmp := d.knownPath() + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, d.knownPath())
		}
	}
	if err != nil {
		d.log.Warn("cannot write known-volume file", "path", d.knownPath(), "err", err)
	}
}

// knownVolume answers Get from the file when the array cannot be asked.
func (d *Driver) knownVolume(dockerName string, cause error) (*plugin.Volume, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	an, ok := d.known[dockerName]
	if !ok {
		return nil, false
	}
	v := &plugin.Volume{
		Name: dockerName,
		Status: map[string]any{
			"array_name":        an,
			"array_unreachable": cause.Error(),
			"transport":         d.tr.Name(),
		},
	}
	if st, ok := d.mounts[an]; ok {
		v.Mountpoint = st.mountpoint
		v.Status["device"] = st.dev
	}
	return v, true
}
