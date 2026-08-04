// Package config loads and normalizes the sdrctl configuration.
//
// The configuration intentionally holds only static facts: node identity,
// device registry, service units, API/MQTT settings. Runtime state lives in
// systemd (active = actual mode, enabled = desired mode), never in files.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPath is the default location of the config file on the node.
const DefaultPath = "/etc/sdrctl/config.yaml"

// Defaults for configurable settings — each value lives here and nowhere
// else in the code; anything network- or path-shaped must be one of these
// or come from the config file.
const (
	DefaultAPIListen          = "0.0.0.0:8081"
	DefaultSocketPath         = "/run/sdrctl/sdrctl.sock"
	DefaultSocketGroup        = "sdrctl"
	defaultModeSetTimeoutSec  = 15
	defaultRestoreCooldownSec = 30
	defaultObserverInterval   = 5
)

// Reserved mode names that cannot be used as service (mode) keys.
var reservedModes = map[string]bool{"idle": true, "conflict": true, "unknown": true}

type Config struct {
	Node     NodeConfig     `yaml:"node"`
	API      APIConfig      `yaml:"api"`
	Socket   SocketConfig   `yaml:"socket"`
	MQTT     MQTTConfig     `yaml:"mqtt"`
	Observer ObserverConfig `yaml:"observer"`

	// ModeSetTimeoutSec bounds one mode transition (disable competitors,
	// enable target, verify the outcome). Used by the agent's executors and
	// by the CLI's direct-systemctl fallback.
	ModeSetTimeoutSec int `yaml:"mode_set_timeout_sec"`

	// Services is single-device shorthand: when Devices is empty, these
	// services are attached to one implicit default device.
	Services map[string]ServiceConfig `yaml:"services"`
	Devices  []DeviceConfig           `yaml:"devices"`

	// Path the config was loaded from (informational).
	Path string `yaml:"-"`
	// Loaded is false when the file was missing and defaults are in use.
	Loaded bool `yaml:"-"`
	// secretFiles records which files supplied which secrets, so the agent
	// can refuse to run with a leaky one (CheckSecretPerms).
	secretFiles []secretSource
}

type secretSource struct {
	path string
	api  bool // supplied api.token
	mqtt bool // supplied mqtt credentials

	// Identity of the ACTUALLY LOADED inode, captured via fstat on the
	// open descriptor before reading — so the permission check cannot be
	// bypassed by swapping the file between check and read.
	regular bool
	mode    os.FileMode
	uid     int
	uidOK   bool // owner could be determined on this platform
}

// effectiveUID is indirect so tests can simulate an owner mismatch without
// root privileges.
var effectiveUID = os.Geteuid

// readSecretBearing opens a file that may carry secrets and returns its
// content together with the identity of the loaded inode.
func readSecretBearing(path string) ([]byte, secretSource, error) {
	src := secretSource{path: path}
	f, err := os.Open(path)
	if err != nil {
		return nil, src, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, src, err
	}
	src.regular = fi.Mode().IsRegular()
	src.mode = fi.Mode().Perm()
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		src.uid = int(st.Uid)
		src.uidOK = true
	}
	data, err := io.ReadAll(f)
	return data, src, err
}

type NodeConfig struct {
	ID   string `yaml:"id"`
	Role string `yaml:"role"`
}

type APIConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Listen       string `yaml:"listen"`
	WriteEnabled bool   `yaml:"write_enabled"`
	Token        string `yaml:"token"`
}

// SocketConfig describes the local control socket of the agent. The socket
// is always served while the agent runs; access control is the socket file's
// ownership (root:<group> 0660), not a token.
type SocketConfig struct {
	Path  string `yaml:"path"`
	Group string `yaml:"group"`
}

type MQTTConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Broker       string `yaml:"broker"`
	ClientID     string `yaml:"client_id"`
	TopicPrefix  string `yaml:"topic_prefix"`
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	QoS          byte   `yaml:"qos"`
	Retain       bool   `yaml:"retain"`
	HeartbeatSec int    `yaml:"heartbeat_sec"`
}

type ObserverConfig struct {
	IntervalSec int `yaml:"interval_sec"`
	// AutoRestore lets the agent restart a service that is enabled (desired)
	// but inactive/failed while its device is present — e.g. after a dongle
	// was re-plugged and systemd's StartLimit had been exhausted.
	AutoRestore bool `yaml:"auto_restore"`
	// RestoreCooldownSec limits auto-restore attempts per device so the
	// agent never fights systemd's own StartLimit throttling.
	RestoreCooldownSec int `yaml:"restore_cooldown_sec"`
}

type ServiceConfig struct {
	Systemd  string `yaml:"systemd"`
	Port     int    `yaml:"port"`
	Optional bool   `yaml:"optional"`
}

type DeviceConfig struct {
	ID           string                   `yaml:"id"`
	Type         string                   `yaml:"type"`
	Label        string                   `yaml:"label"`
	Serial       string                   `yaml:"serial"`
	USBVendorID  string                   `yaml:"usb_vendor_id"`
	USBProductID string                   `yaml:"usb_product_id"`
	Default      bool                     `yaml:"default"`
	Optional     bool                     `yaml:"optional"`
	Services     map[string]ServiceConfig `yaml:"services"`
}

// Load reads the config from path. A missing file is not an error: defaults
// are returned with Loaded=false so CLI commands keep working on a fresh host.
func Load(path string) (*Config, error) {
	cfg := defaults()
	cfg.Path = path

	data, src, err := readSecretBearing(path)
	switch {
	case os.IsNotExist(err):
		cfg.Loaded = false
	case err != nil:
		return nil, fmt.Errorf("read config %s: %w", path, err)
	default:
		// Strict decoding: an unknown field is almost always a typo that
		// would otherwise silently enable a default — the CLI and the
		// agent must read the SAME config, so both fail loudly here.
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
		if err := requireSingleDocument(dec); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
		cfg.Loaded = true
	}

	cfg.normalize()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if cfg.API.Token != "" || cfg.MQTT.Password != "" || cfg.MQTT.Username != "" {
		src.api = cfg.API.Token != ""
		src.mqtt = cfg.MQTT.Password != "" || cfg.MQTT.Username != ""
		cfg.secretFiles = append(cfg.secretFiles, src)
	}
	return cfg, nil
}

// requireSingleDocument rejects a second YAML document: everything after a
// `---` separator would be decoded into nowhere and silently ignored —
// exactly the class of quiet misconfiguration strict parsing exists to
// prevent.
func requireSingleDocument(dec *yaml.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("unexpected second YAML document — everything after '---' would be silently ignored")
	}
	return nil
}

// SecretsPath is the agent-only overlay next to the main config file.
func (c *Config) SecretsPath() string {
	return filepath.Join(filepath.Dir(c.Path), "secrets.yaml")
}

// LoadSecrets merges the agent-only secrets overlay (0600 root:root) into
// the config: api.token and mqtt credentials. Only `sdrctl agent` calls
// this — the operator-readable main config no longer needs to carry
// secrets, which resolves the "CLI must read it, agent must hide it"
// conflict. A missing overlay is fine; an unreadable one is an error (the
// agent runs as root, so this signals a real problem, not a permission
// model at work).
func (c *Config) LoadSecrets() error {
	path := c.SecretsPath()
	data, src, err := readSecretBearing(path)
	switch {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return fmt.Errorf("read secrets %s: %w", path, err)
	}

	var s struct {
		API struct {
			Token string `yaml:"token"`
		} `yaml:"api"`
		MQTT struct {
			Username string `yaml:"username"`
			Password string `yaml:"password"`
		} `yaml:"mqtt"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("parse secrets %s: %w", path, err)
	}
	if err := requireSingleDocument(dec); err != nil {
		return fmt.Errorf("secrets %s: %w", path, err)
	}

	if s.API.Token != "" {
		c.API.Token = s.API.Token
		src.api = true
	}
	if s.MQTT.Username != "" {
		c.MQTT.Username = s.MQTT.Username
		src.mqtt = true
	}
	if s.MQTT.Password != "" {
		c.MQTT.Password = s.MQTT.Password
		src.mqtt = true
	}
	if src.api || src.mqtt {
		c.secretFiles = append(c.secretFiles, src)
	}
	return nil
}

// CheckSecretPerms refuses files that supply secrets the agent will
// actually use unless they are regular files, OWNED by the agent's
// effective user, and closed to group/world. Mode bits alone are not
// enough: a 0600 secrets.yaml owned by a regular local user is readable
// AND replaceable by that user — the exact sdrctl-group bypass this whole
// mechanism exists to prevent. The identity was captured by fstat on the
// descriptor the secrets were read from, so the verdict applies to the
// actually loaded inode. A refusal, not a warning: nobody reads journald
// warnings, and a leaked bearer token controls the SDR node.
func (c *Config) CheckSecretPerms() error {
	for _, src := range c.secretFiles {
		inUse := (src.api && c.API.WriteEnabled) || (src.mqtt && c.MQTT.Enabled)
		if !inUse {
			continue
		}
		if !src.regular {
			return fmt.Errorf("%s carries active secrets but is not a regular file", src.path)
		}
		if !src.uidOK {
			return fmt.Errorf("%s carries active secrets but its owner could not be determined on this platform", src.path)
		}
		if src.uid != effectiveUID() {
			return fmt.Errorf(
				"%s carries active secrets but is owned by uid %d, not the agent user (uid %d) — that user can read AND replace them; chown it to the agent user",
				src.path, src.uid, effectiveUID())
		}
		if src.mode&0o077 != 0 {
			return fmt.Errorf(
				"%s carries active secrets but is readable beyond its owner (%04o); move them to %s with mode 0600 (install -m 0600) or tighten the file",
				src.path, src.mode, c.SecretsPath())
		}
	}
	return nil
}

func defaults() *Config {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "sdr-node"
	}
	return &Config{
		Node:   NodeConfig{ID: host, Role: "rtl-sdr-backend"},
		API:    APIConfig{Enabled: true, Listen: DefaultAPIListen},
		Socket: SocketConfig{Path: DefaultSocketPath, Group: DefaultSocketGroup},
		MQTT:   MQTTConfig{QoS: 1, Retain: true},
		Observer: ObserverConfig{
			IntervalSec:        defaultObserverInterval,
			AutoRestore:        true,
			RestoreCooldownSec: defaultRestoreCooldownSec,
		},
		ModeSetTimeoutSec: defaultModeSetTimeoutSec,
	}
}

func (c *Config) normalize() {
	if c.Node.ID == "" {
		c.Node.ID = "sdr-node"
	}
	if c.API.Listen == "" {
		c.API.Listen = DefaultAPIListen
	}
	if c.Socket.Path == "" {
		c.Socket.Path = DefaultSocketPath
	}
	if c.Socket.Group == "" {
		c.Socket.Group = DefaultSocketGroup
	}
	if c.Observer.IntervalSec < 1 {
		c.Observer.IntervalSec = defaultObserverInterval
	}
	if c.Observer.RestoreCooldownSec < 1 {
		c.Observer.RestoreCooldownSec = defaultRestoreCooldownSec
	}
	if c.ModeSetTimeoutSec < 1 {
		c.ModeSetTimeoutSec = defaultModeSetTimeoutSec
	}
	if c.MQTT.ClientID == "" {
		c.MQTT.ClientID = "sdrctl-" + c.Node.ID
	}
	if c.MQTT.TopicPrefix == "" {
		c.MQTT.TopicPrefix = "sdr/" + c.Node.ID
	}

	// Single-device shorthand: a top-level services block becomes one
	// implicit default device.
	if len(c.Devices) == 0 && len(c.Services) > 0 {
		c.Devices = []DeviceConfig{{
			ID:       "rtl-sdr-01",
			Type:     "rtl-sdr",
			Label:    "RTL-SDR",
			Default:  true,
			Services: c.Services,
		}}
	}

	for i := range c.Devices {
		d := &c.Devices[i]
		if d.Type == "" {
			d.Type = "rtl-sdr"
		}
		if d.Type == "rtl-sdr" {
			if d.USBVendorID == "" {
				d.USBVendorID = "0bda"
			}
			if d.USBProductID == "" {
				d.USBProductID = "2838"
			}
		}
	}
	if len(c.Devices) == 1 {
		c.Devices[0].Default = true
	}
}

func (c *Config) validate() error {
	seen := map[string]bool{}
	defaults := 0
	for _, d := range c.Devices {
		if d.ID == "" {
			return fmt.Errorf("device without id")
		}
		if seen[d.ID] {
			return fmt.Errorf("duplicate device id %q", d.ID)
		}
		seen[d.ID] = true
		if d.Default {
			defaults++
		}
		for name, s := range d.Services {
			if reservedModes[name] {
				return fmt.Errorf("device %s: service name %q is reserved", d.ID, name)
			}
			if s.Systemd == "" {
				return fmt.Errorf("device %s: service %q has no systemd unit", d.ID, name)
			}
		}
	}
	if defaults > 1 {
		return fmt.Errorf("more than one device marked default")
	}

	// Operational cross-field checks: refuse at load time what would
	// otherwise surface as a late runtime surprise.
	if c.API.Enabled {
		if _, _, err := net.SplitHostPort(c.API.Listen); err != nil {
			return fmt.Errorf("api.listen %q is not host:port: %v", c.API.Listen, err)
		}
	}
	if c.MQTT.Enabled && c.MQTT.Broker == "" {
		return fmt.Errorf("mqtt.enabled is true but mqtt.broker is empty")
	}
	if c.MQTT.QoS > 2 {
		return fmt.Errorf("mqtt.qos %d is invalid (0..2)", c.MQTT.QoS)
	}
	if c.MQTT.HeartbeatSec < 0 {
		return fmt.Errorf("mqtt.heartbeat_sec must not be negative")
	}
	unitOwners := map[string]string{}
	for _, d := range c.Devices {
		for name, s := range d.Services {
			ref := d.ID + "/" + name
			if s.Port < 0 || s.Port > 65535 {
				return fmt.Errorf("%s: port %d out of range", ref, s.Port)
			}
			// One systemd unit = one mode of one device; sharing a unit
			// between modes would make both claim the same actual state.
			if other, dup := unitOwners[s.Systemd]; dup {
				return fmt.Errorf("services %s and %s share systemd unit %s", other, ref, s.Systemd)
			}
			unitOwners[s.Systemd] = ref
		}
	}

	// Devices sharing a VID/PID pair must carry non-empty UNIQUE serials:
	// without them physical attribution is guesswork, and the snapshot
	// would have to hand one dongle to two configurations (SDR-P1-05).
	byIDs := map[string][]DeviceConfig{}
	for _, d := range c.Devices {
		pair := strings.ToLower(d.USBVendorID + ":" + d.USBProductID)
		byIDs[pair] = append(byIDs[pair], d)
	}
	for pair, group := range byIDs {
		if len(group) < 2 {
			continue
		}
		serials := map[string]string{}
		for _, d := range group {
			if d.Serial == "" {
				return fmt.Errorf("device %s: devices sharing USB ids %s must set non-empty unique serials (assign with rtl_eeprom)", d.ID, pair)
			}
			if other, dup := serials[d.Serial]; dup {
				return fmt.Errorf("devices %s and %s share USB ids %s AND serial %q — they cannot be told apart", other, d.ID, pair, d.Serial)
			}
			serials[d.Serial] = d.ID
		}
	}
	return nil
}

// ErrNoDefaultDevice is returned by DefaultDevice when several devices are
// configured and none is marked default.
type ErrNoDefaultDevice struct{}

func (ErrNoDefaultDevice) Error() string {
	return "multiple SDR devices configured. Specify device id:\n  sdrctl device <id> mode set <mode>"
}

// DefaultDevice returns the device global commands operate on.
func (c *Config) DefaultDevice() (*DeviceConfig, error) {
	if len(c.Devices) == 0 {
		return nil, fmt.Errorf("no SDR devices configured (see %s)", c.Path)
	}
	for i := range c.Devices {
		if c.Devices[i].Default {
			return &c.Devices[i], nil
		}
	}
	return nil, ErrNoDefaultDevice{}
}

// RequireForAgent enforces the daemon's stricter contract: a real config
// file and at least one device. The soft "missing file → defaults" path
// stays for read-only CLI commands on fresh hosts, but an agent running on
// defaults would report an empty node as healthy and mislead monitoring.
func (c *Config) RequireForAgent() error {
	if !c.Loaded {
		return fmt.Errorf("config %s not found — the agent refuses to run on defaults (override for dev: --allow-empty-config)", c.Path)
	}
	if len(c.Devices) == 0 {
		return fmt.Errorf("config %s defines no SDR devices — an empty agent would look healthy to monitoring (override for dev: --allow-empty-config)", c.Path)
	}
	return nil
}

// ModeSetTimeout returns ModeSetTimeoutSec as a duration, falling back to
// the default when the config was built by hand without normalization.
func (c *Config) ModeSetTimeout() time.Duration {
	if c.ModeSetTimeoutSec < 1 {
		return defaultModeSetTimeoutSec * time.Second
	}
	return time.Duration(c.ModeSetTimeoutSec) * time.Second
}

// RestoreCooldown returns Observer.RestoreCooldownSec as a duration with the
// same hand-built-config fallback.
func (c *Config) RestoreCooldown() time.Duration {
	if c.Observer.RestoreCooldownSec < 1 {
		return defaultRestoreCooldownSec * time.Second
	}
	return time.Duration(c.Observer.RestoreCooldownSec) * time.Second
}

// ModeNames lists the selectable modes of a device: its services plus idle.
func (d *DeviceConfig) ModeNames() []string {
	names := make([]string, 0, len(d.Services)+1)
	for name := range d.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return append(names, "idle")
}

// ResolveMode turns what a human typed into a mode name of this device.
//
// The spellings people actually use are accepted: the underlying binaries
// are called rtl_tcp and rtl_433, so `rtl_tcp` must reach the `rtl-tcp`
// mode, and an unambiguous shorthand (`tcp`, `433`) should work like it
// does in every other modern CLI. Ambiguity is never guessed — it is
// reported with the candidates. Failure always lists what IS available:
// an error that only says "unknown" makes the user guess twice.
func (d *DeviceConfig) ResolveMode(input string) (string, error) {
	modes := d.ModeNames()
	norm := func(s string) string {
		return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "_", "-")
	}
	want := norm(input)
	if want == "" {
		return "", fmt.Errorf("no mode given (available: %s)", strings.Join(modes, ", "))
	}

	var prefix, substr []string
	for _, m := range modes {
		switch n := norm(m); {
		case n == want:
			return m, nil // exact wins outright
		case strings.HasPrefix(n, want):
			prefix = append(prefix, m)
		case strings.Contains(n, want):
			substr = append(substr, m)
		}
	}
	for _, cand := range [][]string{prefix, substr} {
		if len(cand) == 1 {
			return cand[0], nil
		}
		if len(cand) > 1 {
			return "", fmt.Errorf("mode %q is ambiguous: %s", input, strings.Join(cand, ", "))
		}
	}
	return "", fmt.Errorf("unknown mode %q (available: %s)", input, strings.Join(modes, ", "))
}

// DeviceByID looks a device up by its logical id.
func (c *Config) DeviceByID(id string) (*DeviceConfig, error) {
	for i := range c.Devices {
		if c.Devices[i].ID == id {
			return &c.Devices[i], nil
		}
	}
	return nil, fmt.Errorf("unknown device %q", id)
}
