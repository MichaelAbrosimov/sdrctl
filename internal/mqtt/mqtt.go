// Package mqtt publishes node state to an MQTT broker.
//
// Telemetry is strictly one-way: sdrctl only publishes, it never subscribes.
// Commands reach the node exclusively through the HTTP API. State topics are
// retained so a new subscriber sees the current state immediately; the broker
// announces node death itself via the Last Will on .../availability.
package mqtt

import (
	"encoding/json"
	"log"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/MichaelAbrosimov/sdrctl/internal/config"
	"github.com/MichaelAbrosimov/sdrctl/internal/core"
)

type Publisher struct {
	cfg    config.MQTTConfig
	latest func() core.Snapshot
	cli    paho.Client
}

// New prepares a publisher; latest supplies the snapshot published on
// (re)connect so retained topics are correct from the first second.
func New(cfg config.MQTTConfig, latest func() core.Snapshot) *Publisher {
	p := &Publisher{cfg: cfg, latest: latest}

	opts := paho.NewClientOptions().
		AddBroker(cfg.Broker).
		SetClientID(cfg.ClientID).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(10*time.Second).
		SetWill(cfg.TopicPrefix+"/availability", "offline", cfg.QoS, true)
	if cfg.Username != "" {
		opts.SetUsername(cfg.Username)
		opts.SetPassword(cfg.Password)
	}
	opts.SetOnConnectHandler(func(c paho.Client) {
		log.Printf("mqtt: connected to %s", cfg.Broker)
		c.Publish(cfg.TopicPrefix+"/availability", cfg.QoS, true, "online")
		p.PublishSnapshot(core.Snapshot{}, p.latest())
	})
	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		log.Printf("mqtt: connection lost: %v", err)
	})

	p.cli = paho.NewClient(opts)
	return p
}

// Start connects asynchronously; paho keeps retrying in the background, and
// a dead broker never affects the node itself.
func (p *Publisher) Start() { p.cli.Connect() }

func (p *Publisher) Close() {
	if p.cli.IsConnectionOpen() {
		tok := p.cli.Publish(p.cfg.TopicPrefix+"/availability", p.cfg.QoS, true, "offline")
		if !tok.WaitTimeout(time.Second) {
			log.Printf("mqtt: timed out publishing offline availability")
		} else if err := tok.Error(); err != nil {
			log.Printf("mqtt: publish offline availability: %v", err)
		}
	}
	p.cli.Disconnect(250)
}

func (p *Publisher) Connected() bool { return p.cli.IsConnectionOpen() }

func (p *Publisher) publish(topic string, retain bool, payload any) {
	if !p.cli.IsConnectionOpen() {
		return
	}
	var data []byte
	switch v := payload.(type) {
	case string:
		data = []byte(v)
	default:
		var err error
		if data, err = json.Marshal(v); err != nil {
			return
		}
	}
	tok := p.cli.Publish(topic, p.cfg.QoS, retain && p.cfg.Retain, data)
	// Fire-and-forget stays fire-and-forget for the control plane, but the
	// operator gets to SEE a failed publish: wait out of band and log.
	go func() {
		tok.Wait()
		if err := tok.Error(); err != nil {
			log.Printf("mqtt: publish %s: %v", topic, err)
		}
	}()
}

// PublishSnapshot pushes retained state topics and, when prev is a real
// snapshot, change events. Wired to Observer.OnChange.
func (p *Publisher) PublishSnapshot(prev, cur core.Snapshot) {
	prefix := p.cfg.TopicPrefix
	p.publish(prefix+"/status", true, cur)
	p.publish(prefix+"/health", true, cur.Health)

	prevDevs := map[string]core.DeviceStatus{}
	for _, d := range prev.Devices {
		prevDevs[d.ID] = d
	}
	initial := prev.Node == ""

	for _, d := range cur.Devices {
		p.publish(prefix+"/devices/"+d.ID+"/mode", true, d.Mode)
		p.publish(prefix+"/devices/"+d.ID+"/health", true, d.Health)
		if initial {
			continue
		}
		old, ok := prevDevs[d.ID]
		if !ok {
			continue
		}
		if old.Mode != d.Mode {
			p.publish(prefix+"/events", false, map[string]any{
				"ts": cur.GeneratedAt, "device": d.ID,
				"type": "mode_changed", "from": old.Mode, "to": d.Mode,
			})
		}
		if old.Health != d.Health {
			p.publish(prefix+"/events", false, map[string]any{
				"ts": cur.GeneratedAt, "device": d.ID,
				"type": "health_changed", "from": old.Health, "to": d.Health,
			})
		}
	}
}
