// Package driver implements the Docker volume plugin API on top of a
// FlashArray and a block transport.
package driver

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/config"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/flasharray"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/mounter"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/plugin"
	"github.com/ChangemakerStudios/docker-volume-flasharray/internal/transport"
)

// Driver is the plugin.Driver implementation.
type Driver struct {
	cfg   *config.Config
	array flasharray.Array
	tr    transport.Transport
	mnt   mounter.Mounter
	log   *slog.Logger

	host  string // FlashArray host object name for this node
	ports []flasharray.Port

	mu    sync.Mutex // guards locks, mounts and names maps
	locks map[string]*sync.Mutex
	// mounts is keyed by array volume name.
	mounts map[string]*mountState
	// names caches dockerName -> array volume name once resolved.
	names map[string]string
}

type mountState struct {
	dockerName string
	serial     string
	dev        string
	mountpoint string
	refs       map[string]struct{}
}

// Deps lets tests inject fakes.
type Deps struct {
	Array     flasharray.Array
	Transport transport.Transport
	Mounter   mounter.Mounter
	Logger    *slog.Logger
}

// New builds a driver and prepares the host object / sessions.
func New(ctx context.Context, cfg *config.Config, d Deps) (*Driver, error) {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	drv := &Driver{
		cfg:    cfg,
		array:  d.Array,
		tr:     d.Transport,
		mnt:    d.Mounter,
		log:    d.Logger.With("component", "driver"),
		locks:  map[string]*sync.Mutex{},
		mounts: map[string]*mountState{},
		names:  map[string]string{},
	}
	iqns, nqns, err := drv.tr.InitiatorIDs()
	if err != nil {
		return nil, err
	}
	drv.host, err = drv.array.EnsureHost(ctx, cfg.HostName, iqns, nqns)
	if err != nil {
		return nil, fmt.Errorf("ensure host object: %w", err)
	}
	drv.log.Info("host object ready", "host", drv.host, "iqns", iqns, "nqns", nqns)

	if err := os.MkdirAll(cfg.MountRoot, 0o755); err != nil {
		return nil, err
	}
	drv.recoverMounts()

	if cfg.ConnectOnStart {
		if err := drv.ensureConnected(ctx); err != nil {
			// Not fatal: the array may be unreachable at boot; we retry on first mount.
			drv.log.Warn("initial transport connect failed; will retry on first mount", "err", err)
		}
	}
	return drv, nil
}

// ensureConnected refreshes the port list and logs in to every eligible portal.
func (d *Driver) ensureConnected(ctx context.Context) error {
	ports, err := d.array.Ports(ctx)
	if err != nil {
		return fmt.Errorf("list array ports: %w", err)
	}
	d.ports = ports
	return d.tr.Connect(ctx, ports)
}

// recoverMounts rebuilds in-memory state for volumes still mounted from a
// previous plugin process (plugin upgrade/restart with running containers).
func (d *Driver) recoverMounts() {
	entries, err := os.ReadDir(d.cfg.MountRoot)
	if err != nil {
		d.log.Error("cannot scan mount root; existing mounts will not be recovered", "mount_root", d.cfg.MountRoot, "err", err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mp := filepath.Join(d.cfg.MountRoot, e.Name())
		ok, err := d.mnt.IsMounted(mp)
		if err != nil {
			d.log.Error("cannot check mount; not recovered", "mountpoint", mp, "err", err)
			continue
		}
		if !ok {
			continue
		}
		an := d.arrayName(e.Name())
		d.mounts[an] = &mountState{dockerName: e.Name(), mountpoint: mp, refs: map[string]struct{}{}}
		d.log.Info("recovered existing mount", "volume", e.Name(), "mountpoint", mp)
	}
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9-]+`)

// arrayName maps a Docker volume name onto the FlashArray object-name charset
// under this node's namespace. Names that survive sanitisation unchanged map
// 1:1; anything else gets a short hash suffix so distinct Docker names can't
// collide (e.g. "a_b" vs "a-b").
func (d *Driver) arrayName(dockerName string) string {
	san := strings.Trim(unsafeChars.ReplaceAllString(dockerName, "-"), "-")
	name := d.cfg.Namespace + "-" + san
	if san != dockerName || len(name) > 63 {
		sum := sha1.Sum([]byte(dockerName))
		suffix := "-" + hex.EncodeToString(sum[:])[:8]
		limit := 63 - len(suffix)
		if len(name) > limit {
			name = strings.TrimRight(name[:limit], "-")
		}
		name += suffix
	}
	return name
}

func (d *Driver) tagValue(dockerName string) string { return d.cfg.Namespace + "/" + dockerName }

// resolve maps a Docker volume name to its array volume name. The dvfa:name
// tag is authoritative — that is what lets an imported volume keep whatever
// name it already has on the array — and the computed name is the fallback
// for volumes that don't exist yet.
func (d *Driver) resolve(ctx context.Context, dockerName string) string {
	d.mu.Lock()
	an, ok := d.names[dockerName]
	d.mu.Unlock()
	if ok {
		return an
	}
	an, err := d.array.LookupNameTag(ctx, d.tagValue(dockerName))
	if err != nil {
		if !flasharray.IsNotFound(err) {
			d.log.Warn("tag lookup failed; using computed array name", "volume", dockerName, "err", err)
		}
		return d.arrayName(dockerName)
	}
	d.remember(dockerName, an)
	return an
}

func (d *Driver) remember(dockerName, arrayName string) {
	d.mu.Lock()
	d.names[dockerName] = arrayName
	d.mu.Unlock()
}

func (d *Driver) forget(dockerName string) {
	d.mu.Lock()
	delete(d.names, dockerName)
	d.mu.Unlock()
}

// cachedName is resolve() without the array round-trip, for Path().
func (d *Driver) cachedName(dockerName string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if an, ok := d.names[dockerName]; ok {
		return an
	}
	return d.arrayName(dockerName)
}

func (d *Driver) lock(name string) func() {
	d.mu.Lock()
	l, ok := d.locks[name]
	if !ok {
		l = &sync.Mutex{}
		d.locks[name] = l
	}
	d.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (d *Driver) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d.cfg.AttachTimeout+30*time.Second)
}

// Create implements plugin.Driver.
func (d *Driver) Create(req *plugin.CreateRequest) error {
	ctx, cancel := d.ctx()
	defer cancel()

	size := d.cfg.DefaultSize
	importName := ""
	for k, v := range req.Options {
		switch strings.ToLower(k) {
		case "size":
			s, err := config.ParseSize(v)
			if err != nil {
				return fmt.Errorf("option size: %w", err)
			}
			size = s
		case "import":
			importName = v
		default:
			return fmt.Errorf("unknown option %q (supported: size, import)", k)
		}
	}
	if importName != "" {
		return d.importVolume(ctx, req.Name, importName)
	}

	an := d.resolve(ctx, req.Name)
	unlock := d.lock(an)
	defer unlock()

	if v, err := d.array.GetVolume(ctx, an); err == nil {
		if v.Destroyed {
			return fmt.Errorf("volume %s exists on the array in destroyed (pending eradication) state; recover or eradicate it first", an)
		}
		d.log.Info("volume already exists", "volume", req.Name, "array_name", an)
		return nil
	} else if !flasharray.IsNotFound(err) {
		return err
	}

	v, err := d.array.CreateVolume(ctx, an, size)
	if err != nil {
		return fmt.Errorf("create volume %s: %w", an, err)
	}
	if err := d.array.SetNameTag(ctx, an, d.tagValue(req.Name)); err != nil {
		d.log.Error("tagging failed; volume will not appear in list until tagged", "array_name", an, "err", err)
	}
	d.remember(req.Name, an)
	d.log.Info("created volume", "volume", req.Name, "array_name", an, "serial", v.Serial, "bytes", size)
	return nil
}

// importVolume adopts an existing array volume under a Docker name without
// renaming or touching its data: it only writes the dvfa:name tag.
func (d *Driver) importVolume(ctx context.Context, dockerName, arrayName string) error {
	unlock := d.lock(arrayName)
	defer unlock()
	return Adopt(ctx, d.array, d.cfg.Namespace, dockerName, arrayName, d.log)
}

// Adopt tags array volume arrayName as <namespace>/<dockerName>. It refuses
// to steal a volume already claimed by a different Docker name and is a
// no-op if the tag is already correct. Shared by Create(import=) and the
// `adopt` CLI subcommand.
func Adopt(ctx context.Context, array flasharray.Array, namespace, dockerName, arrayName string, log *slog.Logger) error {
	if dockerName == "" || arrayName == "" {
		return errors.New("adopt: docker name and array name are required")
	}
	value := namespace + "/" + dockerName
	v, err := array.GetVolume(ctx, arrayName)
	if err != nil {
		return fmt.Errorf("adopt %s: %w", arrayName, err)
	}
	if v.Destroyed {
		return fmt.Errorf("adopt %s: volume is destroyed on the array", arrayName)
	}
	// Is this Docker name already pointing somewhere?
	if existing, err := array.LookupNameTag(ctx, value); err == nil && existing != arrayName {
		return fmt.Errorf("adopt: docker volume %q already maps to array volume %s", dockerName, existing)
	} else if err != nil && !flasharray.IsNotFound(err) {
		return err
	}
	// Is this array volume already claimed by another Docker name in this namespace?
	tags, err := array.ListNameTags(ctx, namespace+"/")
	if err != nil {
		return err
	}
	if cur, ok := tags[arrayName]; ok {
		if cur == value {
			return nil
		}
		return fmt.Errorf("adopt: array volume %s is already tagged %q", arrayName, cur)
	}
	if err := array.SetNameTag(ctx, arrayName, value); err != nil {
		return fmt.Errorf("adopt %s: %w", arrayName, err)
	}
	if log != nil {
		log.Info("adopted volume", "volume", dockerName, "array_name", arrayName, "serial", v.Serial, "bytes", v.Provisioned)
	}
	return nil
}

// Remove implements plugin.Driver.
func (d *Driver) Remove(req *plugin.RemoveRequest) error {
	ctx, cancel := d.ctx()
	defer cancel()
	an := d.resolve(ctx, req.Name)
	unlock := d.lock(an)
	defer unlock()

	d.mu.Lock()
	_, mounted := d.mounts[an]
	d.mu.Unlock()
	if mounted {
		return fmt.Errorf("volume %s is mounted on this node", req.Name)
	}
	conns, err := d.array.ListConnections(ctx, an)
	if err != nil && !flasharray.IsNotFound(err) {
		return err
	}
	for _, c := range conns {
		if c.Host != d.host {
			return fmt.Errorf("volume %s is connected to host %s; unmount it there first", req.Name, c.Host)
		}
	}
	for _, c := range conns {
		if err := d.array.Disconnect(ctx, c.Host, an); err != nil {
			return err
		}
	}
	if err := d.array.DestroyVolume(ctx, an, d.cfg.EradicateOnRemove); err != nil {
		if flasharray.IsNotFound(err) {
			return nil
		}
		return err
	}
	d.forget(req.Name)
	d.log.Info("removed volume", "volume", req.Name, "array_name", an, "eradicated", d.cfg.EradicateOnRemove)
	return nil
}

// Mount implements plugin.Driver.
func (d *Driver) Mount(req *plugin.MountRequest) (*plugin.MountResponse, error) {
	ctx, cancel := d.ctx()
	defer cancel()
	an := d.resolve(ctx, req.Name)
	unlock := d.lock(an)
	defer unlock()

	d.mu.Lock()
	st, ok := d.mounts[an]
	d.mu.Unlock()
	if ok {
		st.refs[req.ID] = struct{}{}
		return &plugin.MountResponse{Mountpoint: st.mountpoint}, nil
	}

	vol, err := d.array.GetVolume(ctx, an)
	if err != nil {
		return nil, fmt.Errorf("volume %s: %w", req.Name, err)
	}
	if vol.Destroyed {
		return nil, fmt.Errorf("volume %s is destroyed on the array", req.Name)
	}
	if err := d.ensureConnected(ctx); err != nil {
		return nil, err
	}

	// RWO: a block volume can be mounted by one host at a time. Swarm will
	// reschedule a task while a dead node still holds the connection, so by
	// default we take it over (PreemptRWO). A live node writing concurrently
	// would corrupt the filesystem — this is only safe for single-writer use.
	conns, err := d.array.ListConnections(ctx, an)
	if err != nil {
		return nil, err
	}
	for _, c := range conns {
		if c.Host == d.host {
			continue
		}
		if !d.cfg.PreemptRWO {
			return nil, fmt.Errorf("volume %s is attached to host %s (FA_PREEMPT_RWO=false)", req.Name, c.Host)
		}
		d.log.Warn("preempting existing attachment", "volume", req.Name, "from_host", c.Host)
		if err := d.array.Disconnect(ctx, c.Host, an); err != nil {
			return nil, fmt.Errorf("preempt %s from %s: %w", req.Name, c.Host, err)
		}
	}

	lun, err := d.array.Connect(ctx, d.host, an)
	if err != nil {
		return nil, fmt.Errorf("connect %s to %s: %w", req.Name, d.host, err)
	}
	d.log.Info("attached on array", "volume", req.Name, "lun", lun, "serial", vol.Serial)

	dev, err := d.tr.WaitForDevice(ctx, vol.Serial)
	if err != nil {
		d.rollbackAttach(ctx, an, vol.Serial, "")
		return nil, fmt.Errorf("volume %s (serial %s) did not appear on the host: %w", req.Name, vol.Serial, err)
	}

	formatted, err := d.mnt.EnsureFilesystem(ctx, dev, d.cfg.FSType, d.cfg.MkfsOptions)
	if err != nil {
		d.rollbackAttach(ctx, an, vol.Serial, dev)
		return nil, err
	}
	mp := filepath.Join(d.cfg.MountRoot, req.Name)
	if err := d.mnt.Mount(ctx, dev, mp, d.cfg.FSType, d.cfg.MountOptions); err != nil {
		d.rollbackAttach(ctx, an, vol.Serial, dev)
		return nil, err
	}

	st = &mountState{dockerName: req.Name, serial: vol.Serial, dev: dev, mountpoint: mp, refs: map[string]struct{}{req.ID: {}}}
	d.mu.Lock()
	d.mounts[an] = st
	d.mu.Unlock()
	d.log.Info("mounted", "volume", req.Name, "dev", dev, "mountpoint", mp, "formatted", formatted)
	return &plugin.MountResponse{Mountpoint: mp}, nil
}

// rollbackAttach is best effort; failures are logged because a leftover array
// connection makes the next mount elsewhere preempt this host.
func (d *Driver) rollbackAttach(ctx context.Context, an, serial, dev string) {
	if err := d.tr.Detach(ctx, serial, dev); err != nil {
		d.log.Error("rollback: host detach failed", "array_name", an, "serial", serial, "err", err)
	}
	if err := d.array.Disconnect(ctx, d.host, an); err != nil {
		d.log.Error("rollback: array disconnect failed; volume is still connected to this host", "array_name", an, "host", d.host, "err", err)
	}
	if err := d.tr.PostDisconnect(ctx); err != nil {
		d.log.Warn("rollback: post-disconnect failed", "array_name", an, "err", err)
	}
}

// Unmount implements plugin.Driver.
func (d *Driver) Unmount(req *plugin.UnmountRequest) error {
	ctx, cancel := d.ctx()
	defer cancel()
	an := d.resolve(ctx, req.Name)
	unlock := d.lock(an)
	defer unlock()

	d.mu.Lock()
	st, ok := d.mounts[an]
	d.mu.Unlock()
	if !ok {
		// Nothing tracked; make sure the array side is clean anyway.
		return d.array.Disconnect(ctx, d.host, an)
	}
	delete(st.refs, req.ID)
	if len(st.refs) > 0 {
		return nil
	}

	if err := d.mnt.Unmount(ctx, st.mountpoint); err != nil {
		return err
	}
	if err := os.Remove(st.mountpoint); err != nil && !os.IsNotExist(err) {
		d.log.Warn("remove mountpoint dir", "mountpoint", st.mountpoint, "err", err)
	}
	serial := st.serial
	if serial == "" { // recovered mount: look it up
		v, err := d.array.GetVolume(ctx, an)
		if err != nil {
			// Filesystem is already unmounted, so drop the state; a later Mount re-attaches cleanly.
			d.mu.Lock()
			delete(d.mounts, an)
			d.mu.Unlock()
			return fmt.Errorf("volume %s unmounted but serial lookup failed, host devices and array connection left in place: %w", req.Name, err)
		}
		serial = v.Serial
	}
	var errs []error
	if err := d.tr.Detach(ctx, serial, st.dev); err != nil {
		errs = append(errs, err)
	}
	if err := d.array.Disconnect(ctx, d.host, an); err != nil {
		errs = append(errs, err)
	}
	if err := d.tr.PostDisconnect(ctx); err != nil {
		errs = append(errs, err)
	}
	d.mu.Lock()
	delete(d.mounts, an)
	d.mu.Unlock()
	d.log.Info("unmounted", "volume", req.Name)
	return errors.Join(errs...)
}

// Path implements plugin.Driver.
func (d *Driver) Path(req *plugin.PathRequest) (*plugin.PathResponse, error) {
	an := d.cachedName(req.Name)
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.mounts[an]; ok {
		return &plugin.PathResponse{Mountpoint: st.mountpoint}, nil
	}
	return &plugin.PathResponse{}, nil
}

// Get implements plugin.Driver.
func (d *Driver) Get(req *plugin.GetRequest) (*plugin.GetResponse, error) {
	ctx, cancel := d.ctx()
	defer cancel()
	an := d.resolve(ctx, req.Name)
	v, err := d.array.GetVolume(ctx, an)
	if err != nil {
		return nil, fmt.Errorf("volume %s: %w", req.Name, err)
	}
	return &plugin.GetResponse{Volume: d.toVolume(req.Name, an, v)}, nil
}

func (d *Driver) toVolume(dockerName, an string, v *flasharray.Volume) *plugin.Volume {
	out := &plugin.Volume{
		Name:      dockerName,
		CreatedAt: v.Created.Format(time.RFC3339),
		Status: map[string]any{
			"array_name":  an,
			"serial":      v.Serial,
			"provisioned": v.Provisioned,
			"destroyed":   v.Destroyed,
			"transport":   d.tr.Name(),
		},
	}
	d.mu.Lock()
	if st, ok := d.mounts[an]; ok {
		out.Mountpoint = st.mountpoint
		out.Status["device"] = st.dev
	}
	d.mu.Unlock()
	return out
}

// List implements plugin.Driver. Scope is global, so this lists every volume
// in the namespace regardless of which node created it.
func (d *Driver) List() (*plugin.ListResponse, error) {
	ctx, cancel := d.ctx()
	defer cancel()
	prefix := d.cfg.Namespace + "/"
	tags, err := d.array.ListNameTags(ctx, prefix)
	if err != nil {
		return nil, err
	}
	resp := &plugin.ListResponse{}
	d.mu.Lock()
	defer d.mu.Unlock()
	for an, val := range tags {
		d.names[strings.TrimPrefix(val, prefix)] = an
		v := &plugin.Volume{Name: strings.TrimPrefix(val, prefix)}
		if st, ok := d.mounts[an]; ok {
			v.Mountpoint = st.mountpoint
		}
		resp.Volumes = append(resp.Volumes, v)
	}
	return resp, nil
}

// Capabilities implements plugin.Driver.
func (d *Driver) Capabilities() *plugin.CapabilitiesResponse {
	return &plugin.CapabilitiesResponse{Capabilities: plugin.Capability{Scope: "global"}}
}
