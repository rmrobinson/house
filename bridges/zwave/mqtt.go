package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"go.uber.org/zap"

	"github.com/rmrobinson/house/service/bridge"
)

// writeTimeout bounds how long WriteValue waits for a node to echo back an accepted value before
// giving up. See WriteValue's doc comment for why a dead/unreachable node still gets a definitive
// answer (a timeout) rather than hanging. A var, not a const, so tests can shrink it rather than
// spend real wall-clock time exercising the timeout path.
var writeTimeout = 5 * time.Second

// valueID identifies a single Z-Wave value exposed by zwave-js-ui over MQTT, addressed under
// zwave-js-ui's "named topics" scheme (its default, and the only scheme this bridge supports -
// see README's "Architecture" section): <prefix>/[<location>/]<nodeName-or-nodeID_N>/<ccTopicName>
// /endpoint_<N>/<property>[/<propertyKey>], with spaces in property/propertyKey replaced by
// underscores. Confirmed against a real zwave-js-ui v9.1.1 broker (see mqttConn.topicFor's doc
// comment for which pieces are confirmed vs. best-guess). propertyKey is frequently unset (most
// CCs don't use it); the topic omits that segment entirely when empty rather than publishing an
// empty path element.
//
// topicBase (the "[<location>/]<nodeName-or-nodeID_N>" portion) is resolved once per node, at
// build time, via nodeTopicBase - see its doc comment - and carried on every valueID for that
// node so mqttConn.topicFor never needs a side lookup back to the node's nodeInfo.
type valueID struct {
	nodeID       int
	topicBase    string
	commandClass int
	endpoint     int
	property     string
	propertyKey  string
}

// defaultCCTopicNames maps a command class to the topic path segment zwave-js-ui's named-topics
// scheme uses for it. sensor_multilevel/notification/sensor_binary/battery are confirmed against a
// real broker (see mqttConn.topicFor); switch_binary/switch_multilevel are this bridge's best
// guess, following the same "<measurement>_<binary|multilevel>" naming pattern the confirmed
// entries share, but are NOT independently confirmed - there was no live, alive switch/dimmer on
// the network used to validate this bridge to check against. color is taken directly from
// zwave-js-ui's own source (api/lib/Constants.ts's _commandClassMap[0x33] = 'color'), not guessed
// - it does not follow the "<measurement>_<kind>" pattern the switch guesses use. Override via
// mqttConfig.CCTopicNames if a real device proves one of these wrong - see README.
var defaultCCTopicNames = map[int]string{
	ccBinarySwitch:     "switch_binary",
	ccMultilevelSwitch: "switch_multilevel",
	ccColorSwitch:      "color",
	ccMultilevelSensor: "sensor_multilevel",
	ccNotification:     "notification",
	ccBinarySensor:     "sensor_binary",
	ccBattery:          "battery",
}

// slugify converts a property/propertyKey name (as returned by getNodes, e.g. "Air temperature",
// "Motion sensor status") into the form zwave-js-ui uses for the corresponding MQTT topic segment
// - confirmed (against "Air_temperature", "Home_Security/Motion_sensor_status" etc. on a real
// broker) to be nothing more than replacing spaces with underscores; case and punctuation are
// left untouched.
func slugify(s string) string {
	return strings.ReplaceAll(s, " ", "_")
}

// ccTopicName resolves cc to its named-topics path segment, preferring an explicit
// mqttConfig.CCTopicNames override over defaultCCTopicNames.
func (m *mqttConn) ccTopicName(cc int) string {
	if name, ok := m.cfg.CCTopicNames[strconv.Itoa(cc)]; ok {
		return name
	}
	if name, ok := defaultCCTopicNames[cc]; ok {
		return name
	}
	return strconv.Itoa(cc)
}

// topicFor builds the non-/set value topic for v. See valueID's doc comment for the topic shape;
// v.topicBase is resolved once per node at build time (nodeTopicBase), and the command-class
// segment via ccTopicName - the two pieces that differ from zwave-js-ui's alternative numeric-ID
// topic scheme, which this bridge does not support.
func (m *mqttConn) topicFor(v valueID) string {
	parts := []string{
		m.cfg.Prefix,
		v.topicBase,
		m.ccTopicName(v.commandClass),
		fmt.Sprintf("endpoint_%d", v.endpoint),
		slugify(v.property),
	}
	if v.propertyKey != "" {
		parts = append(parts, slugify(v.propertyKey))
	}
	return strings.Join(parts, "/")
}

// mqttConfig describes how to connect to the zwave-js-ui MQTT gateway.
type mqttConfig struct {
	// BrokerURL is a full paho broker URL, e.g. "tcp://localhost:1883" or "ssl://host:8883".
	BrokerURL string `mapstructure:"broker_url"`
	ClientID  string `mapstructure:"client_id"`
	Username  string `mapstructure:"username"`
	Password  string `mapstructure:"password"`

	// Prefix is the MQTT topic prefix zwave-js-ui is configured with (its "mqtt.prefix" setting),
	// with no trailing slash. Unlike an earlier version of this bridge, a location segment is NOT
	// folded into this - zwave-js-ui's named-topics scheme puts a per-node location between the
	// prefix and the node name, and that varies per node (see nodeTopicBase), so it can't be
	// treated as a deployment-wide constant.
	Prefix string `mapstructure:"prefix"`
	// GatewayName is zwave-js-ui's configured gateway name, used to address the
	// <prefix>/_CLIENTS/ZWAVE_GATEWAY-<name>/api/... request/response topics for getNodes.
	GatewayName string `mapstructure:"gateway_name"`

	// CCTopicNames overrides defaultCCTopicNames, keyed by command class number as a string (e.g.
	// {"37": "switch_binary"}). Needed only if a real device proves one of the best-guess defaults
	// (switch_binary/switch_multilevel) wrong - see defaultCCTopicNames.
	CCTopicNames map[string]string `mapstructure:"cc_topic_names"`

	CACertFile         string `mapstructure:"ca_cert_file"`
	InsecureSkipVerify bool   `mapstructure:"insecure_skip_verify"`
}

// apiResponse is the envelope zwave-js-ui's _CLIENTS/.../api/<method> response topics publish.
type apiResponse struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Result  json.RawMessage `json:"result"`
}

// mqttConn owns the single MQTT connection to the zwave-js-ui gateway shared by every node this
// bridge manages, plus the one-shot-topic-wait primitive WriteValue and getNodes are both built
// on. Deliberately kept free of any zwave-specific parameter beyond valueID's topic encoding, per
// the design doc's note that a future zigbee2mqtt bridge would want the same connection/subscribe
// wrapper and one-shot-await primitive, just with different topic semantics layered on top.
type mqttConn struct {
	logger *zap.Logger
	client mqtt.Client
	cfg    mqttConfig

	// onMessage is invoked for every message arriving on the standing <prefix>/# subscription,
	// after any one-shot waiters for that exact topic have already been notified. Installed by
	// network.go for ongoing state routing; nil until then (discovery via getNodes doesn't need
	// it).
	onMessage func(topic string, payload []byte)
	// onConnect is invoked once the standing subscriptions are active, on every (re)connect.
	// Installed by network.go to (re)run discovery.
	onConnect func()

	mu      sync.Mutex
	waiters map[string][]chan []byte
}

func newMQTTConn(logger *zap.Logger, cfg mqttConfig) (*mqttConn, error) {
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
		return nil, fmt.Errorf("zwave: unable to read ca_cert_file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("zwave: ca_cert_file %q contains no valid certificates", cfg.CACertFile)
	}
	tlsCfg.RootCAs = pool
	return tlsCfg, nil
}

// connect blocks until the initial connection attempt completes (success or failure) or ctx is
// cancelled, whichever comes first. Every reconnect after that is handled by the client's own
// SetConnectRetry/SetAutoReconnect machinery, which re-invokes handleConnect (and so onConnect)
// on every successful (re)connect.
func (m *mqttConn) connect(ctx context.Context) error {
	tok := m.client.Connect()

	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()

	select {
	case <-tok.Done():
		return tok.Error()
	case <-timer.C:
		return fmt.Errorf("zwave: connect to mqtt broker timed out")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// handleConnect (re)establishes the single standing subscription this bridge needs - <prefix>/#,
// which covers both ordinary value/status topics and the gateway's nested
// <prefix>/_CLIENTS/ZWAVE_GATEWAY-<name>/api/... response topics used by callAPI/getNodes, since
// "#" matches every remaining topic level regardless of depth - then invokes onConnect. A prior
// version of this bridge additionally subscribed to apiResponseTopic("+") directly; that filter is
// fully subsumed by <prefix>/#, so every api response was being delivered to two matching
// subscriptions and only avoided double-processing because dispatch deletes a topic's waiter
// entry before delivering to it. Subscribing once removes that reliance on delete-before-deliver
// ordering entirely. paho replaces the previous callback for an identical topic filter on repeat
// Subscribe calls rather than stacking duplicate handlers, so no re-subscribe guard is needed the
// way the ESPHome bridge needs one for its client library.
func (m *mqttConn) handleConnect(client mqtt.Client) {
	m.logger.Info("connected to mqtt broker")

	topic := m.cfg.Prefix + "/#"
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
// value/status routing.
//
// A topic with an active waiter is, today, always a WriteValue echo (the only caller of
// subscribeOnce for a non-api topic): forwarding it to onMessage too would be redundant at best -
// WriteValue's caller already applies the accepted value optimistically once WriteValue returns -
// and actively dangerous at worst. onMessage's caller (networkConn) holds a per-device lock across
// the whole command, including the blocking WriteValue call that produces this exact echo; letting
// dispatch re-enter onMessage for that same topic before WriteValue has returned would mean
// blocking on a lock the current call stack already holds. A real broker connection delivers
// messages on its own goroutine, so this wouldn't deadlock outright, but it would still stall the
// message-processing goroutine behind whatever the in-flight command is doing - skipping the
// redundant delivery avoids both.
//
// The one accuracy cost: if a node rejects a write (WriteValue's valuesEqual check fails), the
// rejected echo's actual value is never applied to the cached device state via this path. That's
// an accepted trade-off - the cache simply stays at its last-known value until the node's next
// independent report - rather than reopening the reentrancy problem to chase full accuracy on an
// uncommon failure path.
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

	if strings.HasPrefix(topic, m.cfg.Prefix+"/_CLIENTS/") {
		return
	}
	if m.onMessage != nil {
		m.onMessage(topic, payload)
	}
}

// subscribeOnce registers interest in the next message published to topic. It deliberately
// doesn't issue a fresh MQTT SUBSCRIBE - it piggybacks on the standing subscriptions
// handleConnect already installs, via dispatch - so concurrent calls for different topics (e.g.
// the Colour builder's parallel per-channel WriteValue calls) impose no extra broker round trips
// and no cross-call serialization. The returned cancel func must be called (typically via defer)
// once the caller is done waiting, whether or not a message arrived, to avoid leaking the waiter
// entry.
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
		return fmt.Errorf("zwave: encode payload for %s: %w", topic, err)
	}
	tok := m.client.Publish(topic, 0, false, b)
	if !tok.WaitTimeout(writeTimeout) {
		return fmt.Errorf("zwave: publish to %s timed out", topic)
	}
	return tok.Error()
}

// WriteValue writes value to v and blocks until the node echoes back an accepted value on v's
// (non-/set) topic, per the design doc's "WriteValue-with-echo" primitive: zwave-js-ui's /set
// path has no structured ack, so an accepted write is only observable as the same value arriving
// back on the plain topic. Because correlation is per-topic rather than per-call, concurrent
// WriteValue calls to *different* valueIds (e.g. the RGB builder's per-channel writes) are safe
// to run in parallel with no shared serialization.
//
// A write to a dead/unreachable node publishes successfully but never receives its echo, so this
// naturally surfaces as bridge.ErrCommandTimeout rather than a false success - no special-casing
// needed for that case.
func (m *mqttConn) WriteValue(ctx context.Context, v valueID, value any) error {
	topic := m.topicFor(v)
	ch, cancel := m.subscribeOnce(topic)
	defer cancel()

	if err := m.publish(topic+"/set", map[string]any{"value": value}); err != nil {
		return err
	}

	select {
	case raw := <-ch:
		if !valuesEqual(raw, value) {
			return fmt.Errorf("zwave: node did not accept value at %s: got %s, want %v", topic, raw, value)
		}
		return nil
	case <-time.After(writeTimeout):
		return bridge.ErrCommandTimeout
	case <-ctx.Done():
		return ctx.Err()
	}
}

// valuesEqual compares a raw JSON value-topic payload against a command's intended value. JSON
// numbers decode as float64 regardless of the Go value's original int/float type, so numeric
// comparison normalizes both sides through that rather than requiring exact Go type equality.
func valuesEqual(raw []byte, want any) bool {
	var got any
	if err := json.Unmarshal(raw, &got); err != nil {
		return false
	}

	switch w := want.(type) {
	case bool:
		b, ok := got.(bool)
		return ok && b == w
	case string:
		s, ok := got.(string)
		return ok && s == w
	case int:
		f, ok := got.(float64)
		return ok && f == float64(w)
	case int32:
		f, ok := got.(float64)
		return ok && f == float64(w)
	case int64:
		f, ok := got.(float64)
		return ok && f == float64(w)
	case float32:
		f, ok := got.(float64)
		return ok && f == float64(w)
	case float64:
		f, ok := got.(float64)
		return ok && f == w
	default:
		return false
	}
}

// apiResponseTopic builds the <prefix>/_CLIENTS/ZWAVE_GATEWAY-<name>/api/<method> topic used for
// zwave-js-ui's request/response RPC calls (getNodes today). Confirmed against a real broker that
// _CLIENTS is nested under the configured prefix, not top-level - an earlier version of this
// bridge omitted the prefix here, which meant getNodes could never complete against a real
// gateway.
func (m *mqttConn) apiResponseTopic(method string) string {
	return fmt.Sprintf("%s/_CLIENTS/ZWAVE_GATEWAY-%s/api/%s", m.cfg.Prefix, m.cfg.GatewayName, method)
}

// callAPI issues a zwave-js-ui _CLIENTS/.../api/<method> request and waits for its response.
// This has the same shared-response-topic quirk as WriteValue's echo (no per-call correlation
// ID), which is why the design doc calls for using it only once, serially, at startup rather than
// concurrently - see getNodes below, the only caller today.
func (m *mqttConn) callAPI(ctx context.Context, method string, args []any, timeout time.Duration) (json.RawMessage, error) {
	topic := m.apiResponseTopic(method)
	ch, cancel := m.subscribeOnce(topic)
	defer cancel()

	if err := m.publish(topic+"/set", map[string]any{"args": args}); err != nil {
		return nil, err
	}

	select {
	case raw := <-ch:
		var resp apiResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("zwave: decode %s response: %w", method, err)
		}
		if !resp.Success {
			return nil, fmt.Errorf("zwave: %s call failed: %s", method, resp.Message)
		}
		return resp.Result, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("zwave: %s call timed out", method)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// getNodesTimeout is generous relative to WriteValue's writeTimeout: getNodes returns the full
// node/value list for the whole network in one response, which can take noticeably longer than a
// single value write, especially on a large or just-restarted network.
var getNodesTimeout = 30 * time.Second

// getNodes calls zwave-js-ui's getNodes API once, returning the full node list with cached
// values. Per the design doc, this is deliberately only ever called once, serially, at startup -
// concurrent calls would share the same response topic with no way to tell them apart.
func (m *mqttConn) getNodes(ctx context.Context) ([]nodeInfo, error) {
	result, err := m.callAPI(ctx, "getNodes", nil, getNodesTimeout)
	if err != nil {
		return nil, err
	}
	var nodes []nodeInfo
	if err := json.Unmarshal(result, &nodes); err != nil {
		return nil, fmt.Errorf("zwave: decode getNodes result: %w", err)
	}
	return nodes, nil
}
