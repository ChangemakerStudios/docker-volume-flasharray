// Package config loads plugin settings from environment variables (set via
// `docker plugin set`) and the array credentials file (bind-mounted from the
// host, never baked into the image).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Transport selects how block devices reach the host.
type Transport string

const (
	TransportISCSI   Transport = "iscsi"
	TransportNVMeTCP Transport = "nvme-tcp"
)

// Array is one FlashArray entry from the credentials file.
type Array struct {
	// Endpoint is the management address, e.g. "10.50.0.5" or "array.example.com".
	Endpoint string `json:"endpoint"`
	// APIToken is a FlashArray API token (Settings > Users > API Tokens).
	APIToken string `json:"apiToken"`
	// InsecureSkipVerify disables TLS verification for self-signed management certs.
	InsecureSkipVerify bool `json:"insecureSkipVerify"`
	// APIVersion pins a REST 2.x version; empty = highest 2.x the array offers.
	APIVersion string `json:"apiVersion,omitempty"`
}

// File is the on-disk credentials file format.
type File struct {
	Arrays []Array `json:"arrays"`
}

// Config is the fully resolved plugin configuration.
type Config struct {
	ConfigPath string
	Array      Array
	// Namespace prefixes volume names on the array. Every node in a swarm
	// must share it so the same Docker name resolves to the same volume.
	Namespace string
	// HostName names this node's FlashArray host object (looked up by
	// initiator ID first, so it only matters on first creation).
	HostName          string
	Transport         Transport
	DefaultSize       int64
	FSType            string
	MkfsOptions       []string
	MountOptions      string
	MountRoot         string
	AllowedCIDRs      []string
	PreemptRWO        bool
	EradicateOnRemove bool
	AttachTimeout     time.Duration
	LogLevel          string
	// ConnectOnStart logs into every array portal when the plugin starts so the
	// first volume mount after boot doesn't pay for a cold login while a
	// container is waiting on it.
	ConnectOnStart bool
}

// FromEnv builds a Config from FA_* environment variables and the credentials file.
func FromEnv() (*Config, error) {
	c := &Config{
		ConfigPath:        getenv("FA_CONFIG", "/etc/docker-volume-flasharray/flasharray.json"),
		Namespace:         getenv("FA_NAMESPACE", "docker"),
		HostName:          getenv("FA_HOST_NAME", ""),
		Transport:         Transport(strings.ToLower(getenv("FA_TRANSPORT", string(TransportISCSI)))),
		FSType:            getenv("FA_FS_TYPE", "xfs"),
		MountOptions:      getenv("FA_MOUNT_OPTS", ""),
		MountRoot:         getenv("FA_MOUNT_ROOT", "/mnt/flasharray"),
		LogLevel:          getenv("FA_LOG_LEVEL", "info"),
		PreemptRWO:        getenvBool("FA_PREEMPT_RWO", true),
		EradicateOnRemove: getenvBool("FA_ERADICATE_ON_REMOVE", false),
		ConnectOnStart:    getenvBool("FA_CONNECT_ON_START", true),
	}

	if err := ValidateNamespace(c.Namespace); err != nil {
		return nil, err
	}
	if c.HostName == "" {
		h, err := os.Hostname()
		if err != nil || h == "" {
			return nil, errors.New("FA_HOST_NAME is empty and hostname is unavailable")
		}
		// FQDNs and underscores are common; the array wants [A-Za-z0-9-].
		c.HostName = strings.ReplaceAll(strings.SplitN(h, ".", 2)[0], "_", "-")
	}
	if err := ValidateNamespace(c.HostName); err != nil {
		return nil, fmt.Errorf("host name: %w", err)
	}

	switch c.Transport {
	case TransportISCSI, TransportNVMeTCP:
	default:
		return nil, fmt.Errorf("FA_TRANSPORT must be %q or %q, got %q", TransportISCSI, TransportNVMeTCP, c.Transport)
	}

	size, err := ParseSize(getenv("FA_DEFAULT_SIZE", "32GiB"))
	if err != nil {
		return nil, fmt.Errorf("FA_DEFAULT_SIZE: %w", err)
	}
	c.DefaultSize = size

	if v := getenv("FA_MKFS_OPTS", ""); v != "" {
		c.MkfsOptions = strings.Fields(v)
	} else if c.FSType == "xfs" {
		c.MkfsOptions = []string{"-q"}
	}

	if v := getenv("FA_ALLOWED_CIDRS", ""); v != "" {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				c.AllowedCIDRs = append(c.AllowedCIDRs, s)
			}
		}
	}

	c.AttachTimeout, err = time.ParseDuration(getenv("FA_ATTACH_TIMEOUT", "60s"))
	if err != nil {
		return nil, fmt.Errorf("FA_ATTACH_TIMEOUT: %w", err)
	}

	f, err := LoadFile(c.ConfigPath)
	if err != nil {
		return nil, err
	}
	c.Array = f.Arrays[0]
	return c, nil
}

// LoadFile reads and validates the credentials file.
func LoadFile(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(f.Arrays) == 0 {
		return nil, fmt.Errorf("%s: no arrays configured", path)
	}
	if len(f.Arrays) > 1 {
		// Multi-array (label-based placement) is a later feature; refuse rather than silently use the first.
		return nil, fmt.Errorf("%s: %d arrays configured, only one is supported", path, len(f.Arrays))
	}
	a := f.Arrays[0]
	if a.Endpoint == "" || a.APIToken == "" {
		return nil, fmt.Errorf("%s: arrays[0] needs endpoint and apiToken", path)
	}
	return &f, nil
}

// ValidateNamespace enforces the FlashArray object-name charset for the prefix.
func ValidateNamespace(ns string) error {
	if ns == "" || len(ns) > 32 {
		return fmt.Errorf("namespace %q must be 1-32 chars", ns)
	}
	for _, r := range ns {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return fmt.Errorf("namespace %q may only contain [A-Za-z0-9-]", ns)
		}
	}
	return nil
}

// ParseSize parses "32GiB", "1G", "500MB", "1073741824" into bytes, rounded up
// to a 512-byte multiple. Both binary (GiB) and decimal (GB) suffixes are
// treated as binary, matching what people mean when sizing block volumes.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, errors.New("empty size")
	}
	mult := int64(1)
	suffixes := []struct {
		suf string
		m   int64
	}{
		{"TIB", 1 << 40}, {"TB", 1 << 40}, {"T", 1 << 40},
		{"GIB", 1 << 30}, {"GB", 1 << 30}, {"G", 1 << 30},
		{"MIB", 1 << 20}, {"MB", 1 << 20}, {"M", 1 << 20},
		{"KIB", 1 << 10}, {"KB", 1 << 10}, {"K", 1 << 10},
		{"B", 1},
	}
	for _, x := range suffixes {
		if strings.HasSuffix(s, x.suf) {
			mult = x.m
			s = strings.TrimSpace(strings.TrimSuffix(s, x.suf))
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	n := int64(f * float64(mult))
	if n < 1<<20 {
		return 0, fmt.Errorf("size %d bytes is below the 1MiB minimum", n)
	}
	if rem := n % 512; rem != 0 {
		n += 512 - rem
	}
	return n, nil
}

func getenv(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func getenvBool(k string, def bool) bool {
	v, ok := os.LookupEnv(k)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
