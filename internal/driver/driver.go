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
	"maps"
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

	mu    sync.Mutex // guards locks, mounts, names and known maps
	locks map[string]*sync.Mutex
	// mounts is keyed by array volume name.
	mounts map[string]*mountState
	// names caches dockerName -> array volume name once resolved.
	names map[string]string
	// known is the persisted dockerName -> array volume map behind the
	// outage fallback in Get and List; see known.go.
	known  map[string]string
	saveMu sync.Mutex
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
		known:  map[string]string{},
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
	drv.loadKnown()
	drv.recoverMounts(ctx)

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
func (d *Driver) recoverMounts(ctx context.Context) {
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
		// Through the tag, so an adopted volume is keyed by its real array name.
		an, _, err := d.resolve(ctx, e.Name())
		if err != nil {
			an = d.arrayName(e.Name())
			d.log.Warn("tag lookup failed; recovering mount under its computed array name", "volume", e.Name(), "array_name", an, "err", err)
		}
		d.mounts[an] = &mountState{dockerName: e.Name(), mountpoint: mp, refs: map[string]struct{}{}}
		d.log.Info("recovered existing mount", "volume", e.Name(), "mountpoint", mp)
	}
}

// FlashArray object names are 1-63 of [A-Za-z0-9_-]; Docker also allows '.'.
var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// arrayName maps a Docker volume name onto the FlashArray object-name charset
// under this node's namespace. Names that survive sanitisation unchanged map
// 1:1, as the Pure plugin's did; anything else gets a short hash suffix so
// distinct Docker names can't collide (e.g. "a.b" vs "a-b").
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
// for volumes nothing is tagged for yet. cached reports a hit in d.names. A
// failed lookup is an error rather than a guess: the computed name may be a
// different volume from the one the tag points at.
func (d *Driver) resolve(ctx context.Context, dockerName string) (an string, cached bool, err error) {
	d.mu.Lock()
	an, ok := d.names[dockerName]
	d.mu.Unlock()
	if ok {
		return an, true, nil
	}
	an, err = d.array.LookupNameTag(ctx, d.tagValue(dockerName))
	if flasharray.IsNotFound(err) {
		return d.arrayName(dockerName), false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("look up array volume for %s: %w", dockerName, err)
	}
	d.remember(dockerName, an)
	return an, false, nil
}

// resolveLocked resolves dockerName, takes its per-volume lock and fetches the
// volume. Scope is global, so another node may have removed and recreated the
// volume since we cached its name; a cached name whose volume is gone or
// destroyed is dropped and resolved again. unlock is always non-nil.
func (d *Driver) resolveLocked(ctx context.Context, dockerName string) (an string, v *flasharray.Volume, unlock func(), err error) {
	an, cached, err := d.resolve(ctx, dockerName)
	if err != nil {
		return "", nil, func() {}, err
	}
	unlock = d.lock(an)
	v, err = d.array.GetVolume(ctx, an)
	if cached && stale(v, err) {
		unlock()
		if an, err = d.reresolve(ctx, dockerName, an); err != nil {
			return "", nil, func() {}, err
		}
		unlock = d.lock(an)
		v, err = d.array.GetVolume(ctx, an)
	}
	return an, v, unlock, err
}

// lookup is resolveLocked without the lock, for Get: Mount can hold the lock
// for the whole attach timeout.
func (d *Driver) lookup(ctx context.Context, dockerName string) (string, *flasharray.Volume, error) {
	an, cached, err := d.resolve(ctx, dockerName)
	if err != nil {
		return "", nil, err
	}
	v, err := d.array.GetVolume(ctx, an)
	if cached && stale(v, err) {
		if an, err = d.reresolve(ctx, dockerName, an); err != nil {
			return "", nil, err
		}
		v, err = d.array.GetVolume(ctx, an)
	}
	return an, v, err
}

func stale(v *flasharray.Volume, err error) bool {
	return flasharray.IsNotFound(err) || err == nil && v.Destroyed
}

func (d *Driver) reresolve(ctx context.Context, dockerName, old string) (string, error) {
	d.forget(dockerName)
	d.log.Info("cached array name is stale; resolving again", "volume", dockerName, "array_name", old)
	an, _, err := d.resolve(ctx, dockerName)
	return an, err
}

func (d *Driver) remember(dockerName, arrayName string) {
	d.mu.Lock()
	d.names[dockerName] = arrayName
	d.mu.Unlock()
	d.setKnown(dockerName, arrayName)
}

// forget drops the resolved name. The known entry is only dropped when the
// volume is really gone (Remove), not on a stale re-resolve.
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
	sizeSet := false
	importName, source := "", ""
	for k, v := range req.Options {
		switch strings.ToLower(k) {
		case "size":
			s, err := config.ParseSize(v)
			if err != nil {
				return fmt.Errorf("option size: %w", err)
			}
			size, sizeSet = s, true
		case "import", "import_from_src": // import_from_src is the Pure plugin's spelling
			importName = v
		case "source":
			source = v
		default:
			return fmt.Errorf("unknown option %q (supported: size, import, import_from_src, source)", k)
		}
	}
	switch {
	case importName != "" && (source != "" || sizeSet):
		return errors.New("option import cannot be combined with source or size")
	case source != "" && sizeSet:
		return errors.New("option size cannot be combined with source; a clone is the size of its source")
	case importName != "":
		return d.importVolume(ctx, req.Name, importName)
	}

	an, v, unlock, err := d.resolveLocked(ctx, req.Name)
	defer unlock()

	if err == nil {
		if v.Destroyed {
			return fmt.Errorf("volume %s exists on the array in destroyed (pending eradication) state; recover or eradicate it first", an)
		}
		d.log.Info("volume already exists", "volume", req.Name, "array_name", an)
		return nil
	} else if !flasharray.IsNotFound(err) {
		return err
	}

	if source != "" {
		v, err = d.cloneVolume(ctx, source, an)
	} else if v, err = d.array.CreateVolume(ctx, an, size); err != nil {
		err = fmt.Errorf("create volume %s: %w", an, err)
	}
	if err != nil {
		return err
	}
	if err := d.array.SetNameTag(ctx, an, d.tagValue(req.Name)); err != nil {
		d.log.Error("tagging failed; volume will not appear in list until tagged", "array_name", an, "err", err)
	}
	d.remember(req.Name, an)
	d.log.Info("created volume", "volume", req.Name, "array_name", an, "serial", v.Serial, "bytes", v.Provisioned, "source", source)
	return nil
}

// cloneVolume copies source to an. source is a Docker volume in this
// namespace or, failing that, an array volume name (the Pure plugin's
// `source=` took array names).
func (d *Driver) cloneVolume(ctx context.Context, source, an string) (*flasharray.Volume, error) {
	src, sv, err := d.lookup(ctx, source)
	if flasharray.IsNotFound(err) && src != source {
		src = source
		sv, err = d.array.GetVolume(ctx, source)
	}
	if err != nil {
		return nil, fmt.Errorf("clone source %s: %w", source, err)
	}
	if sv.Destroyed {
		return nil, fmt.Errorf("clone source %s (%s) is destroyed on the array", source, src)
	}
	v, err := d.array.CopyVolume(ctx, src, an)
	if err != nil {
		return nil, fmt.Errorf("clone %s to %s: %w", src, an, err)
	}
	if err := d.array.SetTag(ctx, an, flasharray.TagKeyClonePending, src); err != nil {
		d.log.Error("marking clone failed; mounting it next to its source may fail on a duplicate filesystem UUID", "array_name", an, "source", src, "err", err)
	}
	return v, nil
}

// mountOptions returns the mount options for an, first giving a pending clone
// its own filesystem UUID: the copy carries its source's, and XFS refuses to
// mount a UUID that is already mounted on the host. If that fails (a dirty log
// from cloning a volume in use) XFS mounts with nouuid, which replays the log,
// and the next mount tries again.
func (d *Driver) mountOptions(ctx context.Context, an, dockerName, dev string) string {
	opts := d.cfg.MountOptions
	tags, err := d.array.GetTags(ctx, an)
	if err != nil {
		d.log.Warn("cannot read volume tags; assuming it is not a pending clone", "volume", dockerName, "array_name", an, "err", err)
		return opts
	}
	if _, ok := tags[flasharray.TagKeyClonePending]; !ok {
		return opts
	}
	if err := d.mnt.RegenerateUUID(ctx, dev, d.cfg.FSType); err != nil {
		if d.cfg.FSType != "xfs" {
			d.log.Warn("could not give clone a new filesystem UUID", "volume", dockerName, "dev", dev, "err", err)
			return opts
		}
		d.log.Warn("could not give clone a new filesystem UUID; mounting with nouuid this time", "volume", dockerName, "dev", dev, "err", err)
		if opts == "" {
			return "nouuid"
		}
		return opts + ",nouuid"
	}
	if err := d.array.DeleteTag(ctx, an, flasharray.TagKeyClonePending); err != nil {
		d.log.Warn("clear clone marker failed; the UUID will be regenerated again on next mount", "array_name", an, "err", err)
	}
	d.log.Info("gave cloned volume a new filesystem UUID", "volume", dockerName, "dev", dev)
	return opts
}

// importVolume adopts an existing array volume under a Docker name without
// renaming or touching its data: it only writes the dvfa:name tag.
func (d *Driver) importVolume(ctx context.Context, dockerName, arrayName string) error {
	unlock := d.lock(arrayName)
	defer unlock()
	if err := Adopt(ctx, d.array, d.cfg.Namespace, dockerName, arrayName, d.log); err != nil {
		return err
	}
	d.remember(dockerName, arrayName)
	return nil
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
	an, _, unlock, err := d.resolveLocked(ctx, req.Name)
	defer unlock()
	if err != nil {
		if flasharray.IsNotFound(err) {
			d.forget(req.Name)
			d.setKnown(req.Name, "")
			return nil
		}
		return err
	}

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
	d.setKnown(req.Name, "")
	d.log.Info("removed volume", "volume", req.Name, "array_name", an, "eradicated", d.cfg.EradicateOnRemove)
	return nil
}

// Mount implements plugin.Driver.
func (d *Driver) Mount(req *plugin.MountRequest) (*plugin.MountResponse, error) {
	ctx, cancel := d.ctx()
	defer cancel()
	an, vol, unlock, err := d.resolveLocked(ctx, req.Name)
	defer unlock()

	// Already mounted here: serve it even if the array lookup failed.
	d.mu.Lock()
	st, ok := d.mounts[an]
	d.mu.Unlock()
	if ok {
		st.refs[req.ID] = struct{}{}
		return &plugin.MountResponse{Mountpoint: st.mountpoint}, nil
	}

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
	if err := d.mnt.Mount(ctx, dev, mp, d.cfg.FSType, d.mountOptions(ctx, an, req.Name, dev)); err != nil {
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
	an, _, err := d.resolve(ctx, req.Name)
	if err != nil {
		return err
	}
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
	if err := d.tr.Detach(ctx, serial, st.dev); err != nil {
		// Disconnecting on the array under a device the host still holds turns
		// a cleanup failure into I/O errors for whatever holds it.
		d.mu.Lock()
		delete(d.mounts, an)
		d.mu.Unlock()
		return fmt.Errorf("volume %s unmounted but host detach failed; array connection left in place: %w", req.Name, err)
	}
	var errs []error
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
	an, v, err := d.lookup(ctx, req.Name)
	if err != nil {
		if !flasharray.IsNotFound(err) {
			if kv, ok := d.knownVolume(req.Name, err); ok {
				d.log.Warn("array unreachable; answering from known volumes so Docker does not substitute a local volume", "volume", req.Name, "array_name", kv.Status["array_name"], "err", err)
				return &plugin.GetResponse{Volume: kv}, nil
			}
		}
		return nil, fmt.Errorf("volume %s: %w", req.Name, err)
	}
	d.setKnown(req.Name, an)
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
		d.mu.Lock()
		byName := maps.Clone(d.known)
		d.mu.Unlock()
		if len(byName) == 0 {
			return nil, err
		}
		d.log.Warn("array unreachable; listing known volumes", "volumes", len(byName), "err", err)
		return d.listResponse(byName), nil
	}
	byName := make(map[string]string, len(tags))
	d.mu.Lock()
	for an, val := range tags {
		name := strings.TrimPrefix(val, prefix)
		d.names[name] = an
		byName[name] = an
	}
	d.mu.Unlock()
	d.replaceKnown(byName)
	return d.listResponse(byName), nil
}

func (d *Driver) listResponse(byName map[string]string) *plugin.ListResponse {
	resp := &plugin.ListResponse{}
	d.mu.Lock()
	defer d.mu.Unlock()
	for name, an := range byName {
		v := &plugin.Volume{Name: name}
		if st, ok := d.mounts[an]; ok {
			v.Mountpoint = st.mountpoint
		}
		resp.Volumes = append(resp.Volumes, v)
	}
	return resp
}

// Capabilities implements plugin.Driver.
func (d *Driver) Capabilities() *plugin.CapabilitiesResponse {
	return &plugin.CapabilitiesResponse{Capabilities: plugin.Capability{Scope: "global"}}
}
