// Package transcript holds the provider-neutral transcript and its output formats.
package transcript

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Response formats a client can ask for.
const (
	FormatJSON        = "json"
	FormatText        = "text"
	FormatVerboseJSON = "verbose_json"
	FormatSRT         = "srt"
	FormatVTT         = "vtt"
)

// ValidFormat reports whether f is a supported response_format.
func ValidFormat(f string) bool {
	switch f {
	case FormatJSON, FormatText, FormatVerboseJSON, FormatSRT, FormatVTT:
		return true
	}
	return false
}

// NeedsTimestamps reports whether a format needs segment timestamps from the upstream.
func NeedsTimestamps(f string) bool {
	return f == FormatVerboseJSON || f == FormatSRT || f == FormatVTT
}

// Transcript is the result of one transcription.
type Transcript struct {
	Text     string    `json:"text"`
	Language string    `json:"language"`
	Duration float64   `json:"duration"`
	Segments []Segment `json:"segments"`
	Words    []Word    `json:"words,omitempty"`
}

// Segment matches the OpenAI verbose_json segment shape.
type Segment struct {
	ID               int     `json:"id"`
	Seek             int     `json:"seek"`
	Start            float64 `json:"start"`
	End              float64 `json:"end"`
	Text             string  `json:"text"`
	Tokens           []int   `json:"tokens"`
	Temperature      float64 `json:"temperature"`
	AvgLogprob       float64 `json:"avg_logprob"`
	CompressionRatio float64 `json:"compression_ratio"`
	NoSpeechProb     float64 `json:"no_speech_prob"`
	Speaker          string  `json:"speaker,omitempty"`
}

// Word matches the OpenAI verbose_json word shape, plus an optional speaker.
type Word struct {
	Word    string  `json:"word"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Speaker string  `json:"speaker,omitempty"`
}

// Render returns the body and content type for a response format.
func (t *Transcript) Render(format string) ([]byte, string) {
	switch format {
	case FormatText:
		return []byte(strings.TrimSpace(t.Text) + "\n"), "text/plain; charset=utf-8"
	case FormatVerboseJSON:
		c := *t
		c.Segments = make([]Segment, len(t.Segments))
		copy(c.Segments, t.Segments)
		for i := range c.Segments {
			if c.Segments[i].Tokens == nil {
				c.Segments[i].Tokens = []int{}
			}
		}
		b, _ := json.Marshal(struct {
			Task string `json:"task"`
			Transcript
		}{"transcribe", c})
		return b, "application/json"
	case FormatSRT:
		return []byte(t.subtitles(false)), "text/plain; charset=utf-8"
	case FormatVTT:
		return []byte(t.subtitles(true)), "text/vtt; charset=utf-8"
	default:
		b, _ := json.Marshal(struct {
			Text string `json:"text"`
		}{t.Text})
		return b, "application/json"
	}
}

func (t *Transcript) subtitles(vtt bool) string {
	segs := t.Segments
	if len(segs) == 0 && strings.TrimSpace(t.Text) != "" {
		segs = []Segment{{Start: 0, End: t.Duration, Text: t.Text}}
	}
	var b strings.Builder
	if vtt {
		b.WriteString("WEBVTT\n\n")
	}
	n := 0
	for _, s := range segs {
		text := strings.TrimSpace(s.Text)
		if text == "" {
			continue
		}
		n++
		if !vtt {
			fmt.Fprintf(&b, "%d\n", n)
		}
		fmt.Fprintf(&b, "%s --> %s\n%s\n\n", stamp(s.Start, vtt), stamp(s.End, vtt), text)
	}
	return b.String()
}

func stamp(sec float64, vtt bool) string {
	if sec < 0 {
		sec = 0
	}
	ms := int64(sec*1000 + 0.5)
	h, ms := ms/3600000, ms%3600000
	m, ms := ms/60000, ms%60000
	s, ms := ms/1000, ms%1000
	sep := ","
	if vtt {
		sep = "."
	}
	return fmt.Sprintf("%02d:%02d:%02d%s%03d", h, m, s, sep, ms)
}

// Token is one timed piece of text: a word, the spacing after it, or an audio event.
type Token struct {
	Text    string
	Start   float64
	End     float64
	Speaker string
	IsWord  bool
}

// MaxGap is the silence between words that starts a new segment.
const MaxGap = 1.0

// SegmentsFromTokens builds segments, splitting on a sentence end, on a
// silence over MaxGap and on a speaker change.
func SegmentsFromTokens(tokens []Token) []Segment {
	var segs []Segment
	var cur *Segment
	var text strings.Builder
	lastEnd, endsSentence := 0.0, false
	flush := func() {
		if cur == nil {
			return
		}
		cur.Text = strings.TrimSpace(text.String())
		if cur.Text != "" {
			cur.ID = len(segs)
			segs = append(segs, *cur)
		}
		cur = nil
		text.Reset()
	}
	for _, tk := range tokens {
		if !tk.IsWord && strings.TrimSpace(tk.Text) == "" {
			if cur != nil {
				text.WriteString(tk.Text)
			}
			continue
		}
		if cur != nil && (endsSentence || tk.Start-lastEnd > MaxGap || tk.Speaker != cur.Speaker) {
			flush()
		}
		if cur == nil {
			cur = &Segment{Start: tk.Start, Speaker: tk.Speaker}
		}
		text.WriteString(tk.Text)
		cur.End = tk.End
		lastEnd = tk.End
		trimmed := strings.TrimSpace(tk.Text)
		endsSentence = strings.HasSuffix(trimmed, ".") || strings.HasSuffix(trimmed, "?") ||
			strings.HasSuffix(trimmed, "!") || strings.HasSuffix(trimmed, "…")
	}
	flush()
	return segs
}
