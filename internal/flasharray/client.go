// Package flasharray is a minimal FlashArray REST 2.x client covering exactly
// what a volume driver needs: volumes, tags, hosts, connections and ports.
package flasharray

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TagNamespace is the FlashArray tag namespace this driver owns. The single
// tag key "name" carries "<driver namespace>/<docker volume name>" so the
// original Docker name survives the array's object-name charset rules.
const (
	TagNamespace = "dvfa"
	TagKeyName   = "name"
	// TagKeyClonePending marks a clone whose filesystem still carries its
	// source's UUID; the value is the source array volume.
	TagKeyClonePending = "clone-pending"
)

// ErrNotFound is returned when a volume/host/connection does not exist.
var ErrNotFound = errors.New("not found")

// Volume is the subset of the FlashArray volume resource we use.
type Volume struct {
	Name        string
	Serial      string
	Provisioned int64
	Destroyed   bool
	Created     time.Time
}

// Connection is a host<->volume connection.
type Connection struct {
	Host   string
	Volume string
	LUN    int
}

// Port is a front-end target port (IQN and/or NQN with a portal address).
type Port struct {
	Name   string
	IQN    string
	NQN    string
	Portal string // ip:port, empty for FC
}

// Array is the interface the driver programs against; *Client implements it
// and tests substitute a fake.
type Array interface {
	GetVolume(ctx context.Context, name string) (*Volume, error)
	CreateVolume(ctx context.Context, name string, size int64) (*Volume, error)
	// CopyVolume creates name as a copy of source (same size and contents).
	CopyVolume(ctx context.Context, source, name string) (*Volume, error)
	DestroyVolume(ctx context.Context, name string, eradicate bool) error
	SetNameTag(ctx context.Context, volume, value string) error
	SetTag(ctx context.Context, volume, key, value string) error
	// GetTags returns key -> value for the volume's tags in TagNamespace.
	GetTags(ctx context.Context, volume string) (map[string]string, error)
	DeleteTag(ctx context.Context, volume, key string) error
	ListNameTags(ctx context.Context, valuePrefix string) (map[string]string, error)
	// LookupNameTag returns the array volume carrying exactly this name tag, or ErrNotFound.
	LookupNameTag(ctx context.Context, value string) (string, error)
	// ListVolumes returns non-destroyed volumes whose array name starts with prefix.
	ListVolumes(ctx context.Context, namePrefix string) ([]*Volume, error)
	EnsureHost(ctx context.Context, name string, iqns, nqns []string) (string, error)
	Connect(ctx context.Context, host, volume string) (int, error)
	Disconnect(ctx context.Context, host, volume string) error
	ListConnections(ctx context.Context, volume string) ([]Connection, error)
	Ports(ctx context.Context) ([]Port, error)
}

// Client talks to one FlashArray.
type Client struct {
	endpoint string
	token    string
	version  string
	http     *http.Client
	log      *slog.Logger

	mu        sync.Mutex
	authToken string

	retryBase time.Duration // backoff unit between retries
}

// Options configure a Client.
type Options struct {
	Endpoint           string
	APIToken           string
	APIVersion         string // empty = negotiate highest 2.x
	InsecureSkipVerify bool
	Timeout            time.Duration
	Logger             *slog.Logger
}

// New creates a client and negotiates the API version. It does not log in
// until the first request.
func New(ctx context.Context, o Options) (*Client, error) {
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: o.InsecureSkipVerify} //nolint:gosec // operator choice for self-signed mgmt certs
	c := &Client{
		endpoint: "https://" + strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(o.Endpoint, "https://"), "http://"), "/"),
		token:    o.APIToken,
		version:  o.APIVersion,
		http:     &http.Client{Transport: tr, Timeout: o.Timeout},
		log:      o.Logger.With("component", "flasharray", "array", o.Endpoint),

		retryBase: time.Second,
	}
	if c.version == "" {
		v, err := c.negotiateVersion(ctx)
		if err != nil {
			return nil, err
		}
		c.version = v
	}
	c.log.Info("using FlashArray REST API", "version", c.version)
	return c, nil
}

// Version returns the negotiated REST API version.
func (c *Client) Version() string { return c.version }

func (c *Client) negotiateVersion(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/api/api_version", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("api_version: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		Version []string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("api_version decode: %w", err)
	}
	var v2 []string
	for _, v := range body.Version {
		if strings.HasPrefix(v, "2.") {
			v2 = append(v2, v)
		}
	}
	if len(v2) == 0 {
		return "", errors.New("array offers no REST 2.x API version")
	}
	sort.Slice(v2, func(i, j int) bool { return minor(v2[i]) < minor(v2[j]) })
	return v2[len(v2)-1], nil
}

func minor(v string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(v, "2."))
	return n
}

func (c *Client) login(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/api/"+c.version+"/login", nil)
	req.Header.Set("api-token", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("login: %s", readErr(resp))
	}
	t := resp.Header.Get("x-auth-token")
	if t == "" {
		return errors.New("login: no x-auth-token in response")
	}
	c.mu.Lock()
	c.authToken = t
	c.mu.Unlock()
	return nil
}

// apiError is the FlashArray error envelope.
type apiError struct {
	Errors []struct {
		Message string `json:"message"`
		Context string `json:"context"`
	} `json:"errors"`
}

func readErr(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var e apiError
	if json.Unmarshal(b, &e) == nil && len(e.Errors) > 0 {
		parts := make([]string, 0, len(e.Errors))
		for _, x := range e.Errors {
			if x.Context != "" {
				parts = append(parts, x.Context+": "+x.Message)
			} else {
				parts = append(parts, x.Message)
			}
		}
		return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.Join(parts, "; "))
	}
	return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
}

// maxAttempts bounds retries of one request (re-login, rate limiting, 5xx).
const maxAttempts = 4

// do performs an authenticated request, logging in on first use and again on
// a 401 (sessions idle out after 30 minutes). 429 is retried for any method;
// 5xx and transport errors only for non-POST, since a POST whose response was
// lost may already have created the object.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	u := c.endpoint + "/api/" + c.version + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	reauthed := false
	for attempt := 1; ; attempt++ {
		c.mu.Lock()
		tok := c.authToken
		c.mu.Unlock()
		if tok == "" {
			if err := c.login(ctx); err != nil {
				return err
			}
			c.mu.Lock()
			tok = c.authToken
			c.mu.Unlock()
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("x-auth-token", tok)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if method != http.MethodPost && attempt < maxAttempts && ctx.Err() == nil {
				c.log.Warn("request failed; retrying", "method", method, "path", path, "attempt", attempt, "err", err)
				if c.backoff(ctx, attempt, "") == nil {
					continue
				}
			}
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
		code := resp.StatusCode
		if code == http.StatusUnauthorized && !reauthed {
			resp.Body.Close()
			reauthed = true
			c.log.Info("session expired; logging in again")
			c.mu.Lock()
			c.authToken = ""
			c.mu.Unlock()
			continue
		}
		retryable := code == http.StatusTooManyRequests || code >= 500 && method != http.MethodPost
		if retryable && attempt < maxAttempts {
			msg := readErr(resp)
			resp.Body.Close()
			c.log.Warn("array returned a retryable error; retrying", "method", method, "path", path, "attempt", attempt, "err", msg)
			if err := c.backoff(ctx, attempt, resp.Header.Get("Retry-After")); err != nil {
				return fmt.Errorf("%s %s: %s", method, path, msg)
			}
			continue
		}
		defer resp.Body.Close()
		if code == http.StatusBadRequest || code == http.StatusNotFound {
			msg := readErr(resp)
			if isNotFound(msg) {
				return fmt.Errorf("%s %s: %w (%s)", method, path, ErrNotFound, msg)
			}
			return fmt.Errorf("%s %s: %s", method, path, msg)
		}
		if code < 200 || code > 299 {
			return fmt.Errorf("%s %s: %s", method, path, readErr(resp))
		}
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
				return fmt.Errorf("%s %s: decode: %w", method, path, err)
			}
		}
		return nil
	}
}

// backoff sleeps attempt*retryBase, or Retry-After when the array sends one
// (capped at 10s), unless ctx ends first.
func (c *Client) backoff(ctx context.Context, attempt int, retryAfter string) error {
	d := time.Duration(attempt) * c.retryBase
	if n, err := strconv.Atoi(retryAfter); err == nil && n > 0 {
		d = min(time.Duration(n)*time.Second, 10*time.Second)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func isNotFound(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "does not exist") || strings.Contains(m, "not found") || strings.Contains(m, "no such")
}

// IsNotFound reports whether err wraps ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// --- volumes ---------------------------------------------------------------

type volumeItem struct {
	Name        string `json:"name"`
	Serial      string `json:"serial"`
	Provisioned int64  `json:"provisioned"`
	Destroyed   bool   `json:"destroyed"`
	Created     int64  `json:"created"` // epoch millis
}

type page[T any] struct {
	Items             []T    `json:"items"`
	ContinuationToken string `json:"continuation_token"`
	MoreItemsRemain   bool   `json:"more_items_remaining"`
}

func (v volumeItem) toVolume() *Volume {
	return &Volume{
		Name:        v.Name,
		Serial:      strings.ToLower(v.Serial),
		Provisioned: v.Provisioned,
		Destroyed:   v.Destroyed,
		Created:     time.UnixMilli(v.Created).UTC(),
	}
}

// GetVolume returns a volume by array name.
func (c *Client) GetVolume(ctx context.Context, name string) (*Volume, error) {
	var out page[volumeItem]
	if err := c.do(ctx, http.MethodGet, "/volumes", url.Values{"names": {name}}, nil, &out); err != nil {
		return nil, err
	}
	if len(out.Items) == 0 {
		return nil, ErrNotFound
	}
	return out.Items[0].toVolume(), nil
}

// CreateVolume creates a thin-provisioned volume of the given size in bytes.
func (c *Client) CreateVolume(ctx context.Context, name string, size int64) (*Volume, error) {
	var out page[volumeItem]
	body := map[string]any{"provisioned": size}
	if err := c.do(ctx, http.MethodPost, "/volumes", url.Values{"names": {name}}, body, &out); err != nil {
		return nil, err
	}
	if len(out.Items) == 0 {
		return nil, errors.New("create volume: empty response")
	}
	return out.Items[0].toVolume(), nil
}

// CopyVolume creates name as a copy of source.
func (c *Client) CopyVolume(ctx context.Context, source, name string) (*Volume, error) {
	var out page[volumeItem]
	body := map[string]any{"source": map[string]string{"name": source}}
	if err := c.do(ctx, http.MethodPost, "/volumes", url.Values{"names": {name}}, body, &out); err != nil {
		return nil, err
	}
	if len(out.Items) == 0 {
		return nil, errors.New("copy volume: empty response")
	}
	return out.Items[0].toVolume(), nil
}

// DestroyVolume destroys a volume (24h eradication pending) and optionally
// eradicates it immediately.
func (c *Client) DestroyVolume(ctx context.Context, name string, eradicate bool) error {
	q := url.Values{"names": {name}}
	if err := c.do(ctx, http.MethodPatch, "/volumes", q, map[string]any{"destroyed": true}, nil); err != nil {
		if !IsNotFound(err) {
			return err
		}
	}
	if !eradicate {
		return nil
	}
	err := c.do(ctx, http.MethodDelete, "/volumes", q, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// --- tags ------------------------------------------------------------------

type tagItem struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
	Value     string `json:"value"`
	Resource  struct {
		Name string `json:"name"`
	} `json:"resource"`
}

// SetNameTag records the "<namespace>/<docker name>" tag on a volume.
func (c *Client) SetNameTag(ctx context.Context, volume, value string) error {
	return c.SetTag(ctx, volume, TagKeyName, value)
}

// SetTag creates or replaces one tag in TagNamespace.
func (c *Client) SetTag(ctx context.Context, volume, key, value string) error {
	body := []map[string]string{{"namespace": TagNamespace, "key": key, "value": value}}
	return c.do(ctx, http.MethodPut, "/volumes/tags/batch", url.Values{"resource_names": {volume}}, body, nil)
}

// GetTags returns the volume's tags in TagNamespace.
func (c *Client) GetTags(ctx context.Context, volume string) (map[string]string, error) {
	var out page[tagItem]
	q := url.Values{"resource_names": {volume}, "namespaces": {TagNamespace}}
	if err := c.do(ctx, http.MethodGet, "/volumes/tags", q, nil, &out); err != nil {
		return nil, err
	}
	res := make(map[string]string, len(out.Items))
	for _, t := range out.Items {
		res[t.Key] = t.Value
	}
	return res, nil
}

// DeleteTag removes one tag; missing is not an error.
func (c *Client) DeleteTag(ctx context.Context, volume, key string) error {
	q := url.Values{"resource_names": {volume}, "namespaces": {TagNamespace}, "keys": {key}}
	err := c.do(ctx, http.MethodDelete, "/volumes/tags", q, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ListNameTags returns arrayVolumeName -> tag value for every volume whose
// name tag starts with valuePrefix (e.g. "sblinuxdev/").
func (c *Client) ListNameTags(ctx context.Context, valuePrefix string) (map[string]string, error) {
	res := map[string]string{}
	q := url.Values{
		"namespaces": {TagNamespace},
		"filter":     {fmt.Sprintf("key='%s'", TagKeyName)},
		"limit":      {"1000"},
	}
	for {
		var out page[tagItem]
		if err := c.do(ctx, http.MethodGet, "/volumes/tags", q, nil, &out); err != nil {
			return nil, err
		}
		for _, t := range out.Items {
			if strings.HasPrefix(t.Value, valuePrefix) {
				res[t.Resource.Name] = t.Value
			}
		}
		if !out.MoreItemsRemain || out.ContinuationToken == "" {
			return res, nil
		}
		q.Set("continuation_token", out.ContinuationToken)
	}
}

// LookupNameTag finds the volume tagged dvfa:name=<value>.
func (c *Client) LookupNameTag(ctx context.Context, value string) (string, error) {
	q := url.Values{
		"namespaces": {TagNamespace},
		"filter":     {fmt.Sprintf("key='%s' and value='%s'", TagKeyName, strings.ReplaceAll(value, "'", "\\'"))},
		"limit":      {"2"},
	}
	var out page[tagItem]
	if err := c.do(ctx, http.MethodGet, "/volumes/tags", q, nil, &out); err != nil {
		return "", err
	}
	switch len(out.Items) {
	case 0:
		return "", ErrNotFound
	case 1:
		return out.Items[0].Resource.Name, nil
	default:
		return "", fmt.Errorf("tag %s=%q is on more than one volume (%s, %s)", TagKeyName, value, out.Items[0].Resource.Name, out.Items[1].Resource.Name)
	}
}

// ListVolumes lists live volumes by array-name prefix.
func (c *Client) ListVolumes(ctx context.Context, namePrefix string) ([]*Volume, error) {
	var res []*Volume
	q := url.Values{
		"filter": {fmt.Sprintf("name='%s*' and destroyed='false'", namePrefix)},
		"limit":  {"1000"},
	}
	for {
		var out page[volumeItem]
		if err := c.do(ctx, http.MethodGet, "/volumes", q, nil, &out); err != nil {
			return nil, err
		}
		for _, v := range out.Items {
			res = append(res, v.toVolume())
		}
		if !out.MoreItemsRemain || out.ContinuationToken == "" {
			return res, nil
		}
		q.Set("continuation_token", out.ContinuationToken)
	}
}

// --- hosts -----------------------------------------------------------------

type hostItem struct {
	Name string   `json:"name"`
	IQNs []string `json:"iqns"`
	NQNs []string `json:"nqns"`
}

// EnsureHost finds the host object owning any of the given initiator IDs, or
// creates one named `name` with them. Returns the host name.
func (c *Client) EnsureHost(ctx context.Context, name string, iqns, nqns []string) (string, error) {
	var filters []string
	for _, i := range iqns {
		filters = append(filters, fmt.Sprintf("iqns='%s'", i))
	}
	for _, n := range nqns {
		filters = append(filters, fmt.Sprintf("nqns='%s'", n))
	}
	if len(filters) == 0 {
		return "", errors.New("EnsureHost: no initiator identifiers")
	}
	var out page[hostItem]
	if err := c.do(ctx, http.MethodGet, "/hosts", url.Values{"filter": {strings.Join(filters, " or ")}}, nil, &out); err != nil {
		return "", err
	}
	if len(out.Items) > 0 {
		h := out.Items[0]
		// Make sure the existing host has every identifier we present (e.g. NQN added later).
		body := map[string]any{}
		if missing := diff(iqns, h.IQNs); len(missing) > 0 {
			body["add_iqns"] = missing
		}
		if missing := diff(nqns, h.NQNs); len(missing) > 0 {
			body["add_nqns"] = missing
		}
		if len(body) > 0 {
			if err := c.do(ctx, http.MethodPatch, "/hosts", url.Values{"names": {h.Name}}, body, nil); err != nil {
				return "", err
			}
		}
		return h.Name, nil
	}
	body := map[string]any{}
	if len(iqns) > 0 {
		body["iqns"] = iqns
	}
	if len(nqns) > 0 {
		body["nqns"] = nqns
	}
	if err := c.do(ctx, http.MethodPost, "/hosts", url.Values{"names": {name}}, body, nil); err != nil {
		return "", err
	}
	return name, nil
}

func diff(want, have []string) []string {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	var out []string
	for _, w := range want {
		if !set[w] {
			out = append(out, w)
		}
	}
	return out
}

// --- connections -----------------------------------------------------------

type connItem struct {
	Host struct {
		Name string `json:"name"`
	} `json:"host"`
	Volume struct {
		Name string `json:"name"`
	} `json:"volume"`
	LUN int `json:"lun"`
}

// Connect connects a volume to a host and returns the LUN. An existing
// connection is treated as success.
func (c *Client) Connect(ctx context.Context, host, volume string) (int, error) {
	q := url.Values{"host_names": {host}, "volume_names": {volume}}
	var out page[connItem]
	err := c.do(ctx, http.MethodPost, "/connections", q, nil, &out)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists") {
		err = c.do(ctx, http.MethodGet, "/connections", q, nil, &out)
	}
	if err != nil {
		return 0, err
	}
	if len(out.Items) == 0 {
		return 0, errors.New("connect: empty response")
	}
	return out.Items[0].LUN, nil
}

// Disconnect removes a host<->volume connection; missing is not an error.
func (c *Client) Disconnect(ctx context.Context, host, volume string) error {
	err := c.do(ctx, http.MethodDelete, "/connections", url.Values{"host_names": {host}, "volume_names": {volume}}, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ListConnections returns every host connected to the volume.
func (c *Client) ListConnections(ctx context.Context, volume string) ([]Connection, error) {
	var out page[connItem]
	if err := c.do(ctx, http.MethodGet, "/connections", url.Values{"volume_names": {volume}}, nil, &out); err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	res := make([]Connection, 0, len(out.Items))
	for _, i := range out.Items {
		res = append(res, Connection{Host: i.Host.Name, Volume: i.Volume.Name, LUN: i.LUN})
	}
	return res, nil
}

// --- ports -----------------------------------------------------------------

type portItem struct {
	Name   string `json:"name"`
	IQN    string `json:"iqn"`
	NQN    string `json:"nqn"`
	Portal string `json:"portal"`
}

// Ports lists front-end target ports that have a portal address.
func (c *Client) Ports(ctx context.Context) ([]Port, error) {
	var out page[portItem]
	if err := c.do(ctx, http.MethodGet, "/ports", nil, nil, &out); err != nil {
		return nil, err
	}
	var res []Port
	for _, p := range out.Items {
		if p.Portal == "" {
			continue
		}
		res = append(res, Port(p))
	}
	return res, nil
}
