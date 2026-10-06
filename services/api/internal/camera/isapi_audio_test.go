package camera

import (
	"strings"
	"testing"
)

// liveChannelXML is a verbatim <StreamingChannel> document captured from a
// live Hikvision DS-2CD camera (GET /ISAPI/Streaming/channels/101), trimmed
// to the fields that matter for the audio toggle. The write path depends on
// the exact shape of this document because the firmware rejects any
// partial document, so the fixture deliberately keeps CR/LF-free bodies and
// the real element ordering.
const liveChannelXML = `<?xml version="1.0" encoding="UTF-8"?>
<StreamingChannel version="2.0" xmlns="http://www.hikvision.com/ver20/XMLSchema">
<id>101</id>
<channelName>Camera 01</channelName>
<enabled>true</enabled>
<Transport><maxPacketSize>1000</maxPacketSize></Transport>
<Video><enabled>true</enabled><videoCodecType>H.265</videoCodecType></Video>
<Audio>
<enabled>true</enabled>
<audioInputChannelID>1</audioInputChannelID>
<audioCompressionType>G.711alaw</audioCompressionType>
</Audio>
</StreamingChannel>`

func TestISAPIApplyAudioEnabled(t *testing.T) {
	t.Run("disables audio in the Audio block", func(t *testing.T) {
		got, changed := isapiApplyAudioEnabled(liveChannelXML, false)
		if !changed {
			t.Fatal("expected changed=true")
		}
		if !strings.Contains(got, "<Audio>\n<enabled>false</enabled>") {
			t.Fatalf("Audio block not disabled:\n%s", got)
		}
		// The video <enabled>true</enabled> must be left alone.
		if !strings.Contains(got, "<Video><enabled>true</enabled>") {
			t.Fatalf("video enabled flag was clobbered:\n%s", got)
		}
		if !strings.Contains(got, "<id>101</id>") || !strings.Contains(got, "<channelName>Camera 01</channelName>") {
			t.Fatalf("document identity lost:\n%s", got)
		}
	})

	t.Run("re-enables audio in the Audio block", func(t *testing.T) {
		disabled, _ := isapiApplyAudioEnabled(liveChannelXML, false)
		got, changed := isapiApplyAudioEnabled(disabled, true)
		if !changed {
			t.Fatal("expected changed=true")
		}
		if !strings.Contains(got, "<Audio>\n<enabled>true</enabled>") {
			t.Fatalf("Audio block not re-enabled:\n%s", got)
		}
	})

	t.Run("idempotent when already in the wanted state", func(t *testing.T) {
		got, changed := isapiApplyAudioEnabled(liveChannelXML, true)
		if !changed {
			t.Fatal("expected changed=true even when value already matches")
		}
		if !strings.Contains(got, "<enabled>true</enabled>") {
			t.Fatalf("unexpected document:\n%s", got)
		}
	})

	t.Run("no Audio block is reported as unchanged", func(t *testing.T) {
		doc := strings.Replace(liveChannelXML, "<Audio>", "<Nope>", 1)
		if _, changed := isapiApplyAudioEnabled(doc, false); changed {
			t.Fatal("expected changed=false when there is no Audio block")
		}
	})

	t.Run("Audio block without an enabled child is skipped", func(t *testing.T) {
		doc := strings.Replace(liveChannelXML,
			"<Audio>\n<enabled>true</enabled>\n<audioInputChannelID>1</audioInputChannelID>\n<audioCompressionType>G.711alaw</audioCompressionType>\n</Audio>",
			"<Audio><audioCompressionType>G.711alaw</audioCompressionType></Audio>", 1)
		if _, changed := isapiApplyAudioEnabled(doc, false); changed {
			t.Fatal("expected changed=false when Audio has no enabled child")
		}
	})

	t.Run("attributes on enabled are tolerated", func(t *testing.T) {
		doc := strings.Replace(liveChannelXML, "<enabled>true</enabled>\n<audioInputChannelID>",
			"<enabled opt=\"x\">true</enabled>\n<audioInputChannelID>", 1)
		got, changed := isapiApplyAudioEnabled(doc, false)
		if !changed {
			t.Fatal("expected changed=true")
		}
		if !strings.Contains(got, "<enabled>false</enabled>") {
			t.Fatalf("enabled not rewritten:\n%s", got)
		}
	})
}

// TestRecordOutputArgsFor pins the pickup→preset mapping: pickup disabled
// MUST select a preset whose ffmpeg args end in -an (see
// frigate/ffmpeg_presets.py PRESETS_RECORD_OUTPUT), otherwise Frigate keeps
// muxing the camera's audio track into new recording segments.
func TestRecordOutputArgsFor(t *testing.T) {
	if got := RecordOutputArgsFor(true); got != "preset-record-generic-audio-aac" {
		t.Fatalf("audio on: got %q", got)
	}
	if got := RecordOutputArgsFor(false); got != "preset-record-generic" {
		t.Fatalf("audio off: got %q", got)
	}
}
