// Package config loads and normalizes the sdrctl configuration.
//
// The configuration intentionally holds only static facts: node identity,
// device registry, service units, API/MQTT settings. Runtime state lives in
// systemd (active = actual mode, enabled = desired mode), never in files.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// DefaultPath is the default location of the config file on the node.
const DefaultPath = "/etc/sdrctl/config.yaml"

// Reserved mode names that cannot be used as service (mode) keys.
var reservedModes = map[string]bool{"idle": true, "conflict": true, "unknown": true}

type Config struct {
	Node     NodeConfig     `yaml:"node"`
	API      APIConfig      `yaml:"api"`
	Socket   SocketConfig   `yaml:"socket"`
	MQTT     MQTTConfig     `yaml:"mqtt"`
	Observer ObserverConfig `yaml:"observer"`

	// Services is single-device shorthand: when Devices is empty, these
	// services are attached to one implicit default device.
	Services map[string]ServiceConfig `yaml:"services"`
	Devices  []DeviceConfig           `yaml:"devices"`

	// Path the config was loaded from (informational).
	Path string `yaml:"-"`
	// Loaded is false when the file was missing and defaults are in use.
	Loaded bool `yaml:"-"`
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

	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		cfg.Loaded = false
	case err != nil:
		return nil, fmt.Errorf("read config %s: %w", path, err)
	default:
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
		cfg.Loaded = true
	}

	cfg.normalize()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

func defaults() *Config {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "sdr-node"
	}
	return &Config{
		Node:   NodeConfig{ID: host, Role: "rtl-sdr-backend"},
		API:    APIConfig{Enabled: true, Listen: "0.0.0.0:8081"},
		Socket: SocketConfig{Path: "/run/sdrctl/sdrctl.sock", Group: "sdrctl"},
		MQTT:   MQTTConfig{QoS: 1, Retain: true},
		Observer: ObserverConfig{
			IntervalSec: 5,
			AutoRestore: true,
		},
	}
}

func (c *Config) normalize() {
	if c.Node.ID == "" {
		c.Node.ID = "sdr-node"
	}
	if c.API.Listen == "" {
		c.API.Listen = "0.0.0.0:8081"
	}
	if c.Socket.Path == "" {
		c.Socket.Path = "/run/sdrctl/sdrctl.sock"
	}
	if c.Socket.Group == "" {
		c.Socket.Group = "sdrctl"
	}
	if c.Observer.IntervalSec < 1 {
		c.Observer.IntervalSec = 5
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

// DeviceByID looks a device up by its logical id.
func (c *Config) DeviceByID(id string) (*DeviceConfig, error) {
	for i := range c.Devices {
		if c.Devices[i].ID == id {
			return &c.Devices[i], nil
		}
	}
	return nil, fmt.Errorf("unknown device %q", id)
}
