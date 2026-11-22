package transcript

import (
	"encoding/json"
	"testing"
)

func sample() *Transcript {
	return &Transcript{
		Text:     " Hello world. How are you?",
		Language: "english",
		Duration: 3661.25,
		Segments: []Segment{
			{ID: 0, Start: 0, End: 1.5, Text: " Hello world."},
			{ID: 1, Start: 1.5, End: 3661.25, Text: " How are you?"},
		},
	}
}

func TestRender(t *testing.T) {
	tr := sample()
	cases := map[string]string{
		FormatJSON: `{"text":" Hello world. How are you?"}`,
		FormatText: "Hello world. How are you?\n",
		FormatSRT:  "1\n00:00:00,000 --> 00:00:01,500\nHello world.\n\n2\n00:00:01,500 --> 01:01:01,250\nHow are you?\n\n",
		FormatVTT:  "WEBVTT\n\n00:00:00.000 --> 00:00:01.500\nHello world.\n\n00:00:01.500 --> 01:01:01.250\nHow are you?\n\n",
	}
	for f, want := range cases {
		got, _ := tr.Render(f)
		if string(got) != want {
			t.Errorf("%s:\n got %q\nwant %q", f, got, want)
		}
	}
	b, ct := tr.Render(FormatVerboseJSON)
	if ct != "application/json" {
		t.Errorf("content type %q", ct)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v["task"] != "transcribe" || v["duration"] != 3661.25 || len(v["segments"].([]any)) != 2 {
		t.Errorf("verbose_json = %s", b)
	}
	if seg := v["segments"].([]any)[0].(map[string]any); seg["tokens"] == nil || seg["no_speech_prob"] == nil {
		t.Errorf("segment lacks OpenAI fields: %v", seg)
	}
}

func TestSubtitlesWithoutSegments(t *testing.T) {
	tr := &Transcript{Text: "Just text", Duration: 2}
	got, _ := tr.Render(FormatSRT)
	if want := "1\n00:00:00,000 --> 00:00:02,000\nJust text\n\n"; string(got) != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestSegmentsFromTokens(t *testing.T) {
	w := func(text string, s, e float64, sp string) Token {
		return Token{Text: text, Start: s, End: e, Speaker: sp, IsWord: true}
	}
	sp := func() Token { return Token{Text: " "} }
	segs := SegmentsFromTokens([]Token{
		w("Hello", 0, 0.4, "a"), sp(), w("there", 0.5, 0.9, "a"), sp(),
		w("Hi", 1.0, 1.2, "b"), sp(), w("again.", 1.3, 1.7, "b"), sp(),
		w("So", 1.8, 2.0, "b"), sp(), w("then", 3.5, 3.9, "b"),
	})
	want := []Segment{
		{ID: 0, Start: 0, End: 0.9, Text: "Hello there", Speaker: "a"},
		{ID: 1, Start: 1.0, End: 1.7, Text: "Hi again.", Speaker: "b"},
		{ID: 2, Start: 1.8, End: 2.0, Text: "So", Speaker: "b"},
		{ID: 3, Start: 3.5, End: 3.9, Text: "then", Speaker: "b"},
	}
	if len(segs) != len(want) {
		t.Fatalf("got %d segments: %+v", len(segs), segs)
	}
	for i := range want {
		g := segs[i]
		if g.ID != want[i].ID || g.Start != want[i].Start || g.End != want[i].End || g.Text != want[i].Text || g.Speaker != want[i].Speaker {
			t.Errorf("segment %d = %+v, want %+v", i, g, want[i])
		}
	}
}
