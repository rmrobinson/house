package policy

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	lua "github.com/yuin/gopher-lua"
)

func TestDetectionVisibleParamsMatches(t *testing.T) {
	person := Detection{Label: "person", Score: 0.7, HasStillImage: true, Zones: []string{"yard"}}

	assert.True(t, DetectionVisibleParams{}.matches(person))
	assert.True(t, DetectionVisibleParams{Label: "person", MinScore: 0.7, RequireStillImage: true, Zone: "yard"}.matches(person))
	assert.False(t, DetectionVisibleParams{Label: "car"}.matches(person))
	assert.False(t, DetectionVisibleParams{MinScore: 0.8}.matches(person))
	assert.False(t, DetectionVisibleParams{Zone: "porch"}.matches(person))

	person.HasStillImage = false
	assert.False(t, DetectionVisibleParams{RequireStillImage: true}.matches(person))
	assert.True(t, DetectionVisibleParams{Label: "person"}.matches(person))
}

func TestDetectionVisibleConditionDrivesPolicy(t *testing.T) {
	home := newFakeHomeAPI()
	e, _ := newTestEngine(t, home)
	RegisterBuiltinConditionTypes(e)

	require.NoError(t, e.Register(&Policy{
		ID:               "p",
		ConditionExpr:    Use("detection.visible", DetectionVisibleParams{DeviceID: "cam", Label: "person", RequireStillImage: true}),
		Script:           `home.notify("seen", nil)`,
		OnConditionFalse: Complete,
	}))
	publish := func() { e.Bus().Publish(Event{Topic: "device.updated.cam"}) }

	// A person without a picture yet, and a car with one, don't count.
	home.setDetections("cam", Detection{Label: "person"}, Detection{Label: "car", HasStillImage: true})
	publish()
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, home.notifyCount())

	home.setDetections("cam", Detection{Label: "person", HasStillImage: true})
	publish()
	require.Eventually(t, func() bool { return home.notifyCount() == 1 }, time.Second, 10*time.Millisecond)

	// Still true on another update: no second fire.
	publish()
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1, home.notifyCount())

	// Gone, then back: fires again.
	home.setDetections("cam")
	publish()
	time.Sleep(50 * time.Millisecond)
	home.setDetections("cam", Detection{Label: "person", HasStillImage: true})
	publish()
	require.Eventually(t, func() bool { return home.notifyCount() == 2 }, time.Second, 10*time.Millisecond)
}

func runWithHome(t *testing.T, home HomeAPI, n NotifyAPI, script string) error {
	t.Helper()
	L := lua.NewState()
	defer L.Close()
	registerHomeTable(L, home, func(string) []string { return nil })
	registerNotifyTable(L, n)
	return L.DoString(script)
}

func TestLuaGetDetectionsAndStillImage(t *testing.T) {
	home := newFakeHomeAPI()
	home.setDetections("cam", Detection{Label: "person", Score: 0.75, EventID: "e1", HasStillImage: true, Zones: []string{"yard"}})
	home.stills = map[string]*StillImage{"cam": {Data: []byte("\xff\xd8\x00jpeg"), ContentType: "image/jpeg", Source: "event_snapshot", EventID: "e1"}}
	n := &fakeNotifyAPI{}

	require.NoError(t, runWithHome(t, home, n, `
		local d = home.getDetections("cam")
		assert(#d == 1 and d[1].label == "person" and d[1].event_id == "e1" and d[1].has_still_image and d[1].zones[1] == "yard")
		local img = home.stillImage("cam", {max_height = 720})
		assert(img.source == "event_snapshot" and img.content_type == "image/jpeg" and #img.data == 7)
		notify.send({to = {"r"}, subject = "s", body = '<img src="cid:c1">', content_type = "text/html",
			attachments = {{filename = "a.jpg", content_type = img.content_type, data = img.data, cid = "c1"}}})
	`))

	require.Equal(t, 1, n.callCount())
	atts := n.call(0).attachments
	require.Len(t, atts, 1)
	assert.Equal(t, NotifyAttachment{Filename: "a.jpg", ContentType: "image/jpeg", Data: []byte("\xff\xd8\x00jpeg"), ContentID: "c1"}, atts[0])
}

func TestLuaStillImageFailureCanBeCaughtAndEmailStillSent(t *testing.T) {
	home := newFakeHomeAPI()
	home.stillErr = errors.New("camera unavailable")
	n := &fakeNotifyAPI{}

	require.NoError(t, runWithHome(t, home, n, `
		local ok, img = pcall(home.stillImage, "cam", {max_height = 720})
		assert(not ok)
		assert(string.find(tostring(img), "camera unavailable", 1, true))
		notify.send({to = {"r"}, subject = "s", body = "no picture"})
	`))
	require.Equal(t, 1, n.callCount())
	assert.Empty(t, n.call(0).attachments)
}

func TestLuaStillImageBadOptionsAndUncaughtFailure(t *testing.T) {
	home := newFakeHomeAPI()
	home.stillErr = errors.New("boom")

	assert.Error(t, runWithHome(t, home, &fakeNotifyAPI{}, `home.stillImage("cam", {max_height = "big"})`))
	assert.Error(t, runWithHome(t, home, &fakeNotifyAPI{}, `home.stillImage("cam")`))
}

func TestNotifySendRejectsBadAttachments(t *testing.T) {
	n := &fakeNotifyAPI{}
	assert.Error(t, runWithHome(t, newFakeHomeAPI(), n, `notify.send({to={"r"}, attachments={{content_type="image/jpeg"}}})`))
	assert.Error(t, runWithHome(t, newFakeHomeAPI(), n, `notify.send({to={"r"}, attachments={"nope"}})`))
	assert.Zero(t, n.callCount())
}
