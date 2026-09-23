package main

import (
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// fakeMQTTClient is a minimal in-memory stand-in for paho's mqtt.Client, built directly against
// its exported interface rather than a real broker connection - copied near-verbatim from
// bridges/zwave/fakemqtt_test.go, since mqttConn only ever talks to the network through the
// mqtt.Client interface regardless of which bridge it belongs to; see that file's doc comment for
// the full rationale.
//
// It has no automatic echo behavior: a test that wants WriteState's echo-wait to succeed must
// call respond to arm the reply before triggering the write, mirroring how a real zigbee2mqtt
// gateway only reports a device's new state once it has actually applied (or at least attempted)
// the write.
type fakeMQTTClient struct {
	mu sync.Mutex

	subs      []fakeSub
	published []fakePublish

	// responses maps an exact publish topic to the message that should be delivered back,
	// synchronously, once a Publish to that topic is observed - see respond.
	responses map[string]fakeResponse
}

type fakeSub struct {
	filter  string
	handler mqtt.MessageHandler
}

type fakePublish struct {
	topic   string
	payload []byte
}

type fakeResponse struct {
	topic   string
	payload []byte
}

func newFakeMQTTClient() *fakeMQTTClient {
	return &fakeMQTTClient{responses: make(map[string]fakeResponse)}
}

// respond arms a synchronous reply: the next (and every subsequent) Publish to publishTopic
// delivers payload on responseTopic to every matching subscription, before Publish returns. This
// runs synchronously on the publishing goroutine specifically so tests don't need extra
// synchronization to know the "broker" has already delivered its echo by the time WriteState's
// blocking publish call returns.
func (f *fakeMQTTClient) respond(publishTopic, responseTopic string, payload []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[publishTopic] = fakeResponse{topic: responseTopic, payload: payload}
}

func (f *fakeMQTTClient) publishedTopics() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	topics := make([]string, len(f.published))
	for i, p := range f.published {
		topics[i] = p.topic
	}
	return topics
}

// deliver injects payload as if it arrived on topic from the broker, to every currently matching
// subscription - used by tests to simulate a retained bridge/devices or state message arriving
// independently of any write this bridge issued.
func (f *fakeMQTTClient) deliver(topic string, payload []byte) {
	f.mu.Lock()
	subs := append([]fakeSub(nil), f.subs...)
	f.mu.Unlock()

	for _, s := range subs {
		if topicMatchesFilter(s.filter, topic) {
			s.handler(f, &fakeMessage{topic: topic, payload: payload})
		}
	}
}

func (f *fakeMQTTClient) IsConnected() bool       { return true }
func (f *fakeMQTTClient) IsConnectionOpen() bool  { return true }
func (f *fakeMQTTClient) Connect() mqtt.Token     { return &fakeToken{} }
func (f *fakeMQTTClient) Disconnect(quiesce uint) {}

func (f *fakeMQTTClient) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	b, _ := payload.([]byte)

	f.mu.Lock()
	f.published = append(f.published, fakePublish{topic: topic, payload: b})
	resp, hasResp := f.responses[topic]
	subs := append([]fakeSub(nil), f.subs...)
	f.mu.Unlock()

	if hasResp {
		for _, s := range subs {
			if topicMatchesFilter(s.filter, resp.topic) {
				s.handler(f, &fakeMessage{topic: resp.topic, payload: resp.payload})
			}
		}
	}
	return &fakeToken{}
}

// Subscribe replaces any existing route for an identical topic filter rather than stacking a
// duplicate, mirroring real paho's router behaviour - see bridges/zwave/fakemqtt_test.go's
// Subscribe doc comment for why this matters for reconnect safety.
func (f *fakeMQTTClient) Subscribe(topic string, qos byte, callback mqtt.MessageHandler) mqtt.Token {
	f.mu.Lock()
	defer f.mu.Unlock()

	for i, s := range f.subs {
		if s.filter == topic {
			f.subs[i].handler = callback
			return &fakeToken{}
		}
	}
	f.subs = append(f.subs, fakeSub{filter: topic, handler: callback})
	return &fakeToken{}
}

func (f *fakeMQTTClient) SubscribeMultiple(filters map[string]byte, callback mqtt.MessageHandler) mqtt.Token {
	for topic := range filters {
		f.Subscribe(topic, 0, callback)
	}
	return &fakeToken{}
}

func (f *fakeMQTTClient) Unsubscribe(topics ...string) mqtt.Token {
	f.mu.Lock()
	defer f.mu.Unlock()
	var kept []fakeSub
	for _, s := range f.subs {
		remove := false
		for _, t := range topics {
			if s.filter == t {
				remove = true
				break
			}
		}
		if !remove {
			kept = append(kept, s)
		}
	}
	f.subs = kept
	return &fakeToken{}
}

func (f *fakeMQTTClient) AddRoute(topic string, callback mqtt.MessageHandler) {
	f.mu.Lock()
	f.subs = append(f.subs, fakeSub{filter: topic, handler: callback})
	f.mu.Unlock()
}

func (f *fakeMQTTClient) OptionsReader() mqtt.ClientOptionsReader {
	return mqtt.ClientOptionsReader{}
}

// topicMatchesFilter reports whether topic matches an MQTT topic filter, supporting the '+'
// single-level and '#' multi-level wildcards this bridge's own subscription (<base_topic>/#)
// uses.
func topicMatchesFilter(filter, topic string) bool {
	fParts := strings.Split(filter, "/")
	tParts := strings.Split(topic, "/")

	for i, fp := range fParts {
		if fp == "#" {
			return true
		}
		if i >= len(tParts) {
			return false
		}
		if fp == "+" {
			continue
		}
		if fp != tParts[i] {
			return false
		}
	}
	return len(fParts) == len(tParts)
}

// fakeToken is an already-completed mqtt.Token: every fake operation is synchronous, so there's
// never anything to actually wait for.
type fakeToken struct{}

func (t *fakeToken) Wait() bool                     { return true }
func (t *fakeToken) WaitTimeout(time.Duration) bool { return true }
func (t *fakeToken) Done() <-chan struct{}          { ch := make(chan struct{}); close(ch); return ch }
func (t *fakeToken) Error() error                   { return nil }

type fakeMessage struct {
	topic   string
	payload []byte
}

func (m *fakeMessage) Duplicate() bool   { return false }
func (m *fakeMessage) Qos() byte         { return 0 }
func (m *fakeMessage) Retained() bool    { return false }
func (m *fakeMessage) Topic() string     { return m.topic }
func (m *fakeMessage) MessageID() uint16 { return 0 }
func (m *fakeMessage) Payload() []byte   { return m.payload }
func (m *fakeMessage) Ack()              {}
