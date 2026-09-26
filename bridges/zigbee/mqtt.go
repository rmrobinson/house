package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"go.uber.org/zap"

	"github.com/rmrobinson/house/service/bridge"
)

// writeTimeout bounds how long WriteState waits for a device to echo back a state update
// reflecting an accepted write before giving up. A var, not a const, so tests can shrink it
// rather than spend real wall-clock time exercising the timeout path. See bridges/zwave/mqtt.go's
// writeTimeout - same rationale, different protocol.
var writeTimeout = 5 * time.Second

// mqttConfig describes how to connect to the zigbee2mqtt gateway.
type mqttConfig struct {
	// BrokerURL is a full paho broker URL, e.g. "tcp://localhost:1883" or "ssl://host:8883".
	BrokerURL string `mapstructure:"broker_url"`
	ClientID  string `mapstructure:"client_id"`
	Username  string `mapstructure:"username"`
	Password  string `mapstructure:"password"`

	// BaseTopic is zigbee2mqtt's configured MQTT base topic (its "mqtt.base_topic" setting,
	// "zigbee2mqtt" by default), with no trailing slash.
	BaseTopic string `mapstructure:"base_topic"`

	CACertFile         string `mapstructure:"ca_cert_file"`
	InsecureSkipVerify bool   `mapstructure:"insecure_skip_verify"`
}

// mqttConn owns the single MQTT connection to the zigbee2mqtt gateway shared by every device this
// bridge manages. Unlike bridges/zwave's mqttConn, there is no per-value topic scheme and no
// separate RPC-style request/response call to make: zigbee2mqtt publishes one JSON object per
// device on <base_topic>/<friendly_name> (covering every property zigbee-herdsman-converters
// currently knows about that device reporting - NOT retained by default, see stateTopic's doc
// comment), and discovery is driven by the retained <base_topic>/bridge/devices array rather than
// an explicit getNodes-style call - see network.go's onMessage/handleBridgeDevices.
type mqttConn struct {
	logger *zap.Logger
	client mqtt.Client
	cfg    mqttConfig

	// onMessage is invoked for every message arriving on the standing <base_topic>/# subscription,
	// after any one-shot waiters for that exact topic have already been notified. Installed by
	// network.go for discovery and ongoing state routing.
	onMessage func(topic string, payload []byte)
	// onConnect is invoked once the standing subscription is active, on every (re)connect. Purely
	// informational here (logging) - unlike bridges/zwave, rediscovery doesn't need to be
	// triggered explicitly from here, since it's driven by the retained bridge/devices message
	// zigbee2mqtt redelivers on every fresh subscribe (see network.go).
	onConnect func()

	mu      sync.Mutex
	waiters map[string][]chan []byte
}

func newMQTTConn(logger *zap.Logger, cfg mqttConfig) (*mqttConn, error) {
	if cfg.BaseTopic == "" {
		cfg.BaseTopic = "zigbee2mqtt"
	}

	m := &mqttConn{
		logger:  logger,
		cfg:     cfg,
		waiters: make(map[string][]chan []byte),
	}

	opts := mqtt.NewClientOptions().
		AddBroker(cfg.BrokerURL).
		SetClientID(cfg.ClientID).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetOnConnectHandler(m.handleConnect).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			m.logger.Warn("lost connection to mqtt broker", zap.Error(err))
		})

	if cfg.Username != "" {
		opts.SetUsername(cfg.Username)
		opts.SetPassword(cfg.Password)
	}

	if cfg.CACertFile != "" || cfg.InsecureSkipVerify {
		tlsCfg, err := buildTLSConfig(cfg)
		if err != nil {
			return nil, err
		}
		opts.SetTLSConfig(tlsCfg)
	}

	m.client = mqtt.NewClient(opts)
	return m, nil
}

func buildTLSConfig(cfg mqttConfig) (*tls.Config, error) {
	tlsCfg := &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify}
	if cfg.CACertFile == "" {
		return tlsCfg, nil
	}

	pem, err := os.ReadFile(cfg.CACertFile)
	if err != nil {
		return nil, fmt.Errorf("zigbee: unable to read ca_cert_file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("zigbee: ca_cert_file %q contains no valid certificates", cfg.CACertFile)
	}
	tlsCfg.RootCAs = pool
	return tlsCfg, nil
}

// connect blocks until the initial connection attempt completes (success or failure) or ctx is
// cancelled, whichever comes first. Every reconnect after that is handled by the client's own
// SetConnectRetry/SetAutoReconnect machinery, which re-invokes handleConnect on every successful
// (re)connect.
func (m *mqttConn) connect(ctx context.Context) error {
	tok := m.client.Connect()

	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()

	select {
	case <-tok.Done():
		return tok.Error()
	case <-timer.C:
		return fmt.Errorf("zigbee: connect to mqtt broker timed out")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// handleConnect (re)establishes the single standing subscription this bridge needs -
// <base_topic>/#, covering device state/availability topics and the bridge/devices discovery
// topic alike - then invokes onConnect. A fresh subscribe (which every reconnect performs, since
// paho's default session is not persistent) causes the broker to immediately redeliver every
// retained message under this filter - but, confirmed against a real broker running zigbee2mqtt's
// default mqtt.retain: false, that's bridge/devices (and a handful of other bridge/* metadata
// topics) only, NOT a device's own state topic. A reconnect therefore recovers the device list
// for free, the same outcome bridges/zwave's onConnect-triggered getNodes call achieves - but NOT
// each device's last-known state, which stays at whatever this bridge last knew (or its trait's
// zero value) until the device's own next report. See stateTopic's doc comment and README's
// "Architecture" section.
func (m *mqttConn) handleConnect(client mqtt.Client) {
	m.logger.Info("connected to mqtt broker")

	topic := m.cfg.BaseTopic + "/#"
	if tok := client.Subscribe(topic, 0, m.handleMessage); tok.Wait() && tok.Error() != nil {
		m.logger.Error("unable to subscribe to topics", zap.String("topic", topic), zap.Error(tok.Error()))
		return
	}

	if m.onConnect != nil {
		m.onConnect()
	}
}

func (m *mqttConn) handleMessage(_ mqtt.Client, msg mqtt.Message) {
	m.dispatch(msg.Topic(), msg.Payload())
}

// dispatch notifies any one-shot waiters registered on topic (see subscribeOnce), then - only if
// nothing was already waiting on this exact topic - hands off to onMessage for ordinary
// discovery/state routing.
//
// A topic with an active waiter is, today, always a WriteState echo (the only caller of
// subscribeOnce): forwarding it to onMessage too would re-enter the per-device lock
// networkConn.applyCommand holds across its own blocking WriteState call - see
// bridges/zwave/mqtt.go's dispatch doc comment for the full reentrancy rationale, which applies
// here unchanged even though zigbee2mqtt's per-device topic carries the device's *entire* state
// rather than a single value. The accepted cost is the same one zwave takes: if the device's
// state topic reports something other than what was asked for, that rejected/differing echo isn't
// applied to the cached device here - the cache stays at its last-known value until the device's
// next independent report.
func (m *mqttConn) dispatch(topic string, payload []byte) {
	m.mu.Lock()
	waiters := m.waiters[topic]
	delete(m.waiters, topic)
	m.mu.Unlock()

	if len(waiters) > 0 {
		for _, ch := range waiters {
			select {
			case ch <- payload:
			default:
			}
		}
		return
	}

	if m.onMessage != nil {
		m.onMessage(topic, payload)
	}
}

// subscribeOnce registers interest in the next message published to topic. It deliberately
// doesn't issue a fresh MQTT SUBSCRIBE - it piggybacks on the standing <base_topic>/#
// subscription handleConnect already installs, via dispatch. The returned cancel func must be
// called (typically via defer) once the caller is done waiting, whether or not a message arrived,
// to avoid leaking the waiter entry.
func (m *mqttConn) subscribeOnce(topic string) (<-chan []byte, func()) {
	ch := make(chan []byte, 1)

	m.mu.Lock()
	m.waiters[topic] = append(m.waiters[topic], ch)
	m.mu.Unlock()

	cancel := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		ws := m.waiters[topic]
		for i, c := range ws {
			if c == ch {
				m.waiters[topic] = append(ws[:i], ws[i+1:]...)
				break
			}
		}
		if len(m.waiters[topic]) == 0 {
			delete(m.waiters, topic)
		}
	}
	return ch, cancel
}

func (m *mqttConn) publish(topic string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("zigbee: encode payload for %s: %w", topic, err)
	}
	tok := m.client.Publish(topic, 0, false, b)
	if !tok.WaitTimeout(writeTimeout) {
		return fmt.Errorf("zigbee: publish to %s timed out", topic)
	}
	return tok.Error()
}

// stateTopic returns the full-state topic zigbee2mqtt publishes for friendlyName. Despite
// carrying a device's complete known state, this topic is NOT retained by default - confirmed
// against a real broker running zigbee2mqtt's default mqtt.retain: false. Only bridge/devices
// (and a handful of other bridge/* metadata topics) are retained by zigbee2mqtt's own convention;
// a deployment would need to explicitly opt into mqtt.retain: true for a fresh subscribe to this
// topic to redeliver a device's last-known state the way it does for bridge/devices.
func (m *mqttConn) stateTopic(friendlyName string) string {
	return m.cfg.BaseTopic + "/" + friendlyName
}

// WriteState publishes set (a partial state object, e.g. {"state":"ON","brightness":150}) to
// friendlyName's /set topic and blocks until the device's next full-state report reflects every
// key in set, per zigbee2mqtt's convention of accepting a partial JSON object on <friendly_name>
// /set and echoing back the applied change - alongside every other currently-known property - on
// the plain <friendly_name> topic. There's no per-write correlation id, so (mirroring
// bridges/zwave's WriteValue) only the first message to arrive after the publish is checked; a
// device that reports an intermediate/unrelated update before its real echo would cause a false
// "did not accept" error rather than a retry - accepted as the same trade-off zwave's WriteValue
// makes for the same reason (avoiding a second, less-precise correlation mechanism), and, per
// zigbee2mqtt's own documented behaviour, not expected in practice for a single in-flight write.
//
// A write to an unreachable/offline device publishes successfully but never receives a matching
// echo, so this naturally surfaces as bridge.ErrCommandTimeout rather than a false success.
func (m *mqttConn) WriteState(ctx context.Context, friendlyName string, set map[string]any) error {
	topic := m.stateTopic(friendlyName)
	ch, cancel := m.subscribeOnce(topic)
	defer cancel()

	if err := m.publish(topic+"/set", set); err != nil {
		return err
	}

	select {
	case raw := <-ch:
		if !stateMatches(raw, set) {
			return fmt.Errorf("zigbee: device %s did not accept state: got %s, want %v", friendlyName, raw, set)
		}
		return nil
	case <-time.After(writeTimeout):
		return bridge.ErrCommandTimeout
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stateMatches reports whether raw (a device's full-state JSON payload) reflects every key/value
// in want, per WriteState's doc comment. Comparison goes through json.Marshal on both sides
// (rather than a hand-written recursive equality check) so a nested composite value - e.g.
// want["color"] = map[string]any{"hue":200,"saturation":50} matched against a decoded
// map[string]interface{} - compares correctly without needing type-specific cases the way
// bridges/zwave's valuesEqual does for its always-scalar values: encoding/json's map key
// ordering is deterministic (sorted), and a JSON number round-trips to the same textual form
// regardless of whether it started as an int or a float64, so byte-equal marshalled output is a
// reliable equality check here.
func stateMatches(raw []byte, want map[string]any) bool {
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		return false
	}

	subset := make(map[string]any, len(want))
	for k := range want {
		v, ok := got[k]
		if !ok {
			return false
		}
		subset[k] = v
	}

	wantJSON, err := json.Marshal(want)
	if err != nil {
		return false
	}
	gotJSON, err := json.Marshal(subset)
	if err != nil {
		return false
	}
	return string(wantJSON) == string(gotJSON)
}

// decodeAvailability decodes a zigbee2mqtt availability-topic payload into an online/offline
// bool. Modern zigbee2mqtt (availability.legacy: false, the default since zigbee2mqtt 1.x)
// publishes a JSON object {"state":"online"|"offline"}; the legacy mode publishes a bare
// "online"/"offline" string with no JSON quoting. Both are decoded leniently - mirroring
// bridges/zwave's decodeFlexString-style tolerance - since this bridge has no live broker to
// confirm which mode a given deployment uses.
func decodeAvailability(raw []byte) (online bool, ok bool) {
	var obj struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.State != "" {
		return strings.EqualFold(obj.State, "online"), true
	}

	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" {
		return false, false
	}
	return strings.EqualFold(s, "online"), true
}
