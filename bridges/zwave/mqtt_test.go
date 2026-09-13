package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/rmrobinson/house/service/bridge"
)

func newTestMQTTConn(t *testing.T, cfg mqttConfig) (*mqttConn, *fakeMQTTClient) {
	t.Helper()
	fc := newFakeMQTTClient()
	mc := &mqttConn{
		logger:  zaptest.NewLogger(t),
		cfg:     cfg,
		client:  fc,
		waiters: make(map[string][]chan []byte),
	}
	// Installs the standing subscriptions dispatch routes through - a real paho client invokes
	// this automatically via SetOnConnectHandler; the fake doesn't simulate that connect
	// machinery, so tests invoke it directly.
	mc.handleConnect(fc)
	return mc, fc
}

// TestTopicFor_MatchesRealBroker confirms the named-topics format this bridge builds matches what
// a real zwave-js-ui v9.1.1 broker actually publishes - not a synthetic assumption. The
// sensor_multilevel/notification/battery names and the endpoint_<N>/space-to-underscore shape were
// read directly off a live broker (see mqttConn.topicFor's doc comment); switch_binary is this
// bridge's unconfirmed best guess for a command class no live device on that broker could exercise.
func TestTopicFor_MatchesRealBroker(t *testing.T) {
	mc, _ := newTestMQTTConn(t, mqttConfig{Prefix: "zwave"})

	cases := []struct {
		name string
		v    valueID
		want string
	}{
		{
			name: "multilevel sensor, confirmed against a real AEON MultiSensor 6",
			v:    valueID{nodeID: 2, topicBase: "second_floor/guest_bedroom/motion_sensor", commandClass: ccMultilevelSensor, endpoint: 0, property: "Air temperature"},
			want: "zwave/second_floor/guest_bedroom/motion_sensor/sensor_multilevel/endpoint_0/Air_temperature",
		},
		{
			name: "notification with a multi-word property and propertyKey, confirmed live",
			v:    valueID{nodeID: 2, topicBase: "second_floor/guest_bedroom/motion_sensor", commandClass: ccNotification, endpoint: 0, property: "Home Security", propertyKey: "Motion sensor status"},
			want: "zwave/second_floor/guest_bedroom/motion_sensor/notification/endpoint_0/Home_Security/Motion_sensor_status",
		},
		{
			name: "battery, confirmed live",
			v:    valueID{nodeID: 2, topicBase: "second_floor/guest_bedroom/motion_sensor", commandClass: ccBattery, endpoint: 0, property: "level"},
			want: "zwave/second_floor/guest_bedroom/motion_sensor/battery/endpoint_0/level",
		},
		{
			name: "unnamed/unlocated node falls back to nodeID_<n>, confirmed live against the controller node",
			v:    valueID{nodeID: 1, topicBase: "nodeID_1", commandClass: ccBattery, endpoint: 0, property: "level"},
			want: "zwave/nodeID_1/battery/endpoint_0/level",
		},
		{
			name: "binary switch - best-guess default, not confirmed against a live device",
			v:    valueID{nodeID: 7, topicBase: "first_floor/porch/exterior_outlet", commandClass: ccBinarySwitch, endpoint: 0, property: "targetValue"},
			want: "zwave/first_floor/porch/exterior_outlet/switch_binary/endpoint_0/targetValue",
		},
		{
			name: "color switch - taken from zwave-js-ui's own source (Constants.ts's _commandClassMap[0x33] = 'color'), not guessed",
			v:    valueID{nodeID: 9, topicBase: "living_room/bulb", commandClass: ccColorSwitch, endpoint: 0, property: "targetColor", propertyKey: "2"},
			want: "zwave/living_room/bulb/color/endpoint_0/targetColor/2",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, mc.topicFor(c.v))
		})
	}
}

func TestCCTopicName_Override(t *testing.T) {
	mc, _ := newTestMQTTConn(t, mqttConfig{Prefix: "zwave", CCTopicNames: map[string]string{"37": "switch_binary_v2"}})
	assert.Equal(t, "switch_binary_v2", mc.ccTopicName(ccBinarySwitch))
	// Unrelated CCs still fall back to the built-in default.
	assert.Equal(t, "battery", mc.ccTopicName(ccBattery))
}

func TestWriteValue_Success(t *testing.T) {
	mc, fc := newTestMQTTConn(t, mqttConfig{Prefix: "zwave"})

	v := valueID{nodeID: 5, topicBase: "nodeID_5", commandClass: ccBinarySwitch, endpoint: 0, property: "targetValue"}
	topic := mc.topicFor(v)
	fc.respond(topic+"/set", topic, []byte("true"))

	err := mc.WriteValue(context.Background(), v, true)
	require.NoError(t, err)
	assert.Contains(t, fc.publishedTopics(), topic+"/set")
}

func TestWriteValue_Timeout(t *testing.T) {
	orig := writeTimeout
	writeTimeout = 50 * time.Millisecond
	defer func() { writeTimeout = orig }()

	mc, _ := newTestMQTTConn(t, mqttConfig{Prefix: "zwave"})
	v := valueID{nodeID: 5, topicBase: "nodeID_5", commandClass: ccBinarySwitch, endpoint: 0, property: "targetValue"}

	// No response armed - simulates a dead/unreachable node: the publish succeeds but no echo
	// ever arrives, per WriteValue's doc comment.
	err := mc.WriteValue(context.Background(), v, true)
	assert.ErrorIs(t, err, bridge.ErrCommandTimeout)
}

func TestWriteValue_RejectedValue(t *testing.T) {
	mc, fc := newTestMQTTConn(t, mqttConfig{Prefix: "zwave"})
	v := valueID{nodeID: 5, topicBase: "nodeID_5", commandClass: ccBinarySwitch, endpoint: 0, property: "targetValue"}
	topic := mc.topicFor(v)

	// The node echoes back false when true was requested - e.g. it rejected the write.
	fc.respond(topic+"/set", topic, []byte("false"))

	err := mc.WriteValue(context.Background(), v, true)
	require.Error(t, err)
	assert.NotErrorIs(t, err, bridge.ErrCommandTimeout)
}

func TestGetNodes(t *testing.T) {
	mc, fc := newTestMQTTConn(t, mqttConfig{Prefix: "zwave", GatewayName: "Home"})

	apiTopic := mc.apiResponseTopic("getNodes")
	payload := []byte(`{"success":true,"result":[{"id":5,"deviceClass":{"generic":16}},{"id":9,"deviceClass":{"generic":17}}]}`)
	fc.respond(apiTopic+"/set", apiTopic, payload)

	nodes, err := mc.getNodes(context.Background())
	require.NoError(t, err)
	require.Len(t, nodes, 2)
	assert.Equal(t, 5, nodes[0].ID)
	assert.Equal(t, genericBinarySwitch, nodes[0].DeviceClass.Generic)
	assert.Equal(t, 9, nodes[1].ID)
}

// TestGetNodes_NumericPropertyAndPropertyKey is a regression test for a real bug found against a
// live zwave-js-ui broker: every Configuration CC (112) value in a node's cached value list -
// present regardless of whether this bridge does anything with CC 112 - encodes "property" and
// "propertyKey" as bare JSON numbers, not strings. A strict string-typed field decode fails
// outright the instant it hits one, which aborted getNodes entirely - for every node, not just the
// one CC 112 value this bridge never reads - until nodeValue.UnmarshalJSON started tolerating both
// encodings.
func TestGetNodes_NumericPropertyAndPropertyKey(t *testing.T) {
	mc, fc := newTestMQTTConn(t, mqttConfig{Prefix: "zwave", GatewayName: "Home"})

	apiTopic := mc.apiResponseTopic("getNodes")
	payload := []byte(`{"success":true,"result":[{"id":2,"deviceClass":{"generic":33},"values":{
		"112-0-48-8":{"commandClass":112,"endpoint":0,"property":48,"propertyKey":8,"value":0},
		"113-0-Home Security-Motion sensor status":{"commandClass":113,"endpoint":0,"property":"Home Security","propertyKey":"Motion sensor status","value":0}
	}}]}`)
	fc.respond(apiTopic+"/set", apiTopic, payload)

	nodes, err := mc.getNodes(context.Background())
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	require.Len(t, nodes[0].Values, 2)
	assert.Equal(t, "48", nodes[0].Values["112-0-48-8"].Property, "a numeric property must decode to its canonical string form")
	assert.Equal(t, "8", nodes[0].Values["112-0-48-8"].PropertyKey)
	assert.Equal(t, "Home Security", nodes[0].Values["113-0-Home Security-Motion sensor status"].Property, "a string property must still decode unchanged")
}

func TestGetNodes_Failure(t *testing.T) {
	mc, fc := newTestMQTTConn(t, mqttConfig{Prefix: "zwave", GatewayName: "Home"})

	apiTopic := mc.apiResponseTopic("getNodes")
	fc.respond(apiTopic+"/set", apiTopic, []byte(`{"success":false,"message":"not ready"}`))

	_, err := mc.getNodes(context.Background())
	require.Error(t, err)
}

// TestAPIResponseTopic_NestedUnderPrefix is a regression test for a real bug found against a live
// broker: _CLIENTS response topics are nested under the configured prefix (e.g.
// "zwave/_CLIENTS/ZWAVE_GATEWAY-<name>/api/<method>"), not top-level as an earlier version of this
// bridge assumed - which meant getNodes could never complete against a real gateway.
func TestAPIResponseTopic_NestedUnderPrefix(t *testing.T) {
	mc, _ := newTestMQTTConn(t, mqttConfig{Prefix: "zwave", GatewayName: "Home"})
	assert.Equal(t, "zwave/_CLIENTS/ZWAVE_GATEWAY-Home/api/getNodes", mc.apiResponseTopic("getNodes"))
}

func TestDispatch_APITopicsNotForwardedToOnMessage(t *testing.T) {
	mc, fc := newTestMQTTConn(t, mqttConfig{Prefix: "zwave", GatewayName: "Home"})

	var got []string
	mc.onMessage = func(topic string, _ []byte) { got = append(got, topic) }

	apiTopic := mc.apiResponseTopic("getNodes")
	fc.respond(apiTopic+"/set", apiTopic, []byte(`{"success":true,"result":[]}`))
	_, err := mc.getNodes(context.Background())
	require.NoError(t, err)

	assert.Empty(t, got, "api response topics must not be routed to onMessage")
}

// TestDispatch_WaitedTopicsNotForwardedToOnMessage covers the fix for a real deadlock: a
// WriteValue echo used to also be forwarded to onMessage, which - through networkConn - needs the
// same per-device lock WriteValue's own caller (networkConn.applyCommand) already holds for the
// whole command. Forwarding a topic that had an active one-shot waiter is now skipped entirely
// (see mqttConn.dispatch's doc comment); this confirms that specifically, independent of any
// networkConn involvement.
func TestDispatch_WaitedTopicsNotForwardedToOnMessage(t *testing.T) {
	mc, fc := newTestMQTTConn(t, mqttConfig{Prefix: "zwave"})

	var got []string
	mc.onMessage = func(topic string, _ []byte) { got = append(got, topic) }

	v := valueID{nodeID: 5, topicBase: "nodeID_5", commandClass: ccBinarySwitch, endpoint: 0, property: "targetValue"}
	valueTopic := mc.topicFor(v)
	fc.respond(valueTopic+"/set", valueTopic, []byte("true"))
	require.NoError(t, mc.WriteValue(context.Background(), v, true))

	assert.Empty(t, got, "a topic with an active WriteValue waiter must not also be forwarded to onMessage")
}

// TestDispatch_UnwaitedTopicsForwardedToOnMessage confirms the fix above didn't overreach: a
// message on a topic with no active waiter - e.g. a device pushing an unsolicited state change,
// not the echo of an in-flight write - must still reach onMessage.
func TestDispatch_UnwaitedTopicsForwardedToOnMessage(t *testing.T) {
	mc, _ := newTestMQTTConn(t, mqttConfig{Prefix: "zwave"})

	var got []string
	mc.onMessage = func(topic string, _ []byte) { got = append(got, topic) }

	valueTopic := mc.topicFor(valueID{nodeID: 5, topicBase: "nodeID_5", commandClass: ccBinarySwitch, endpoint: 0, property: "targetValue"})
	mc.handleMessage(nil, &fakeMessage{topic: valueTopic, payload: []byte("false")})

	assert.Equal(t, []string{valueTopic}, got)
}

func TestValuesEqual(t *testing.T) {
	assert.True(t, valuesEqual([]byte("true"), true))
	assert.False(t, valuesEqual([]byte("false"), true))
	assert.True(t, valuesEqual([]byte("50"), int32(50)))
	assert.True(t, valuesEqual([]byte(`"idle"`), "idle"))
	assert.False(t, valuesEqual([]byte("50"), int32(51)))
}

// writeTestCACertPEM generates a throwaway self-signed CA certificate and writes its PEM encoding
// to a file under t.TempDir(), returning the file's path. Used to exercise buildTLSConfig's
// happy path without depending on any real certificate material.
func writeTestCACertPEM(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "zwave-test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "ca.pem")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}))

	return path
}

// TestBuildTLSConfig_ValidCACert is the happy path buildTLSConfig had zero coverage of: a real
// (if throwaway) CA cert file should load into tlsCfg.RootCAs with no error.
func TestBuildTLSConfig_ValidCACert(t *testing.T) {
	path := writeTestCACertPEM(t)

	tlsCfg, err := buildTLSConfig(mqttConfig{CACertFile: path})
	require.NoError(t, err)
	require.NotNil(t, tlsCfg.RootCAs)
	assert.False(t, tlsCfg.InsecureSkipVerify)
}

// TestBuildTLSConfig_MissingFile confirms a nonexistent ca_cert_file fails closed with a wrapped,
// explanatory error rather than a bare os.ReadFile error or a silently-empty CA pool.
func TestBuildTLSConfig_MissingFile(t *testing.T) {
	_, err := buildTLSConfig(mqttConfig{CACertFile: "/nonexistent/ca.pem"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ca_cert_file")
}

// TestBuildTLSConfig_InvalidPEM confirms a ca_cert_file that exists but contains no valid PEM
// certificate data fails closed instead of silently proceeding with an empty trust pool (which
// would make every server certificate fail verification with a confusing "x509: certificate
// signed by unknown authority" instead of this file's clearer error).
func TestBuildTLSConfig_InvalidPEM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.pem")
	require.NoError(t, os.WriteFile(path, []byte("not a certificate"), 0o600))

	_, err := buildTLSConfig(mqttConfig{CACertFile: path})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no valid certificates")
}

// TestBuildTLSConfig_InsecureSkipVerify_NoCACert confirms insecure_skip_verify alone (no
// ca_cert_file) reaches the resulting tls.Config, and doesn't require a CA file to be set.
func TestBuildTLSConfig_InsecureSkipVerify_NoCACert(t *testing.T) {
	tlsCfg, err := buildTLSConfig(mqttConfig{InsecureSkipVerify: true})
	require.NoError(t, err)
	assert.True(t, tlsCfg.InsecureSkipVerify)
	assert.Nil(t, tlsCfg.RootCAs)
}

// TestNewMQTTConn_InvalidCACertFile confirms newMQTTConn itself - not just buildTLSConfig -
// surfaces a bad ca_cert_file as an error rather than constructing a client with a nil/broken TLS
// config.
func TestNewMQTTConn_InvalidCACertFile(t *testing.T) {
	_, err := newMQTTConn(zaptest.NewLogger(t), mqttConfig{CACertFile: "/nonexistent/ca.pem"})
	require.Error(t, err)
}
