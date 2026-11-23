package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Jamir-boop/stt-router/internal/transcript"
)

type azureDef struct {
	EnhancedMode        azureEnhanced     `json:"enhancedMode"`
	Locales             []string          `json:"locales,omitempty"`
	Diarization         *azureDiarization `json:"diarization,omitempty"`
	PhraseList          *azurePhraseList  `json:"phraseList,omitempty"`
	ProfanityFilterMode string            `json:"profanityFilterMode"`
}

type azureEnhanced struct {
	Enabled      bool              `json:"enabled"`
	Model        string            `json:"model"`
	ModelOptions azureModelOptions `json:"modelOptions"`
}

type azureModelOptions struct {
	Timestamps      string `json:"timestamps"`
	TranscribeStyle string `json:"transcribeStyle,omitempty"`
}

type azureDiarization struct {
	Enabled     bool `json:"enabled"`
	MaxSpeakers int  `json:"maxSpeakers,omitempty"`
}

type azurePhraseList struct {
	Phrases []string `json:"phrases"`
}

// Diarize reports whether the client asked for diarization with diarize=true.
func Diarize(extra []Field) bool {
	for _, f := range extra {
		if f.Name == "diarize" {
			if v, err := strconv.ParseBool(f.Value); err == nil {
				return v
			}
		}
	}
	return false
}

// azureDefinition builds the "definition" JSON part. prompt and temperature have no Azure field.
func azureDefinition(r *Request) ([]byte, error) {
	d := azureDef{ProfanityFilterMode: "None"} // Azure masks profanity by default
	d.EnhancedMode.Enabled = true
	d.EnhancedMode.Model = r.Model
	d.EnhancedMode.ModelOptions.Timestamps = "segment" // segment ends give the duration for usage
	if r.Words || r.Timestamps {
		d.EnhancedMode.ModelOptions.Timestamps = "word"
	}
	if r.Language != "" {
		d.Locales = []string{r.Language}
	}
	if Diarize(r.Extra) {
		d.Diarization = &azureDiarization{Enabled: true}
	}
	for _, f := range r.Extra {
		switch f.Name {
		case "num_speakers":
			n, err := strconv.Atoi(strings.TrimSpace(f.Value))
			if err != nil || n < 1 {
				return nil, &RequestError{"num_speakers must be a positive integer"}
			}
			if d.Diarization != nil {
				d.Diarization.MaxSpeakers = n
			}
		case "keyterms":
			if d.PhraseList == nil {
				d.PhraseList = &azurePhraseList{}
			}
			d.PhraseList.Phrases = append(d.PhraseList.Phrases, f.Value)
		case "transcribe_style":
			// Microsoft Learn puts transcribeStyle next to timestamps in enhancedMode.modelOptions.
			d.EnhancedMode.ModelOptions.TranscribeStyle = f.Value
		}
	}
	return json.Marshal(d)
}

type azureResponse struct {
	DurationMilliseconds float64 `json:"durationMilliseconds"`
	CombinedPhrases      []struct {
		Text string `json:"text"`
	} `json:"combinedPhrases"`
	Phrases []struct {
		OffsetMilliseconds   float64         `json:"offsetMilliseconds"`
		DurationMilliseconds float64         `json:"durationMilliseconds"`
		Text                 string          `json:"text"`
		Locale               string          `json:"locale"`
		Speaker              json.RawMessage `json:"speaker"`
		Words                []struct {
			Text                 string  `json:"text"`
			OffsetMilliseconds   float64 `json:"offsetMilliseconds"`
			DurationMilliseconds float64 `json:"durationMilliseconds"`
		} `json:"words"`
	} `json:"phrases"`
}

// azureSpeaker turns a speaker number (or a string) into "speaker_<n>".
func azureSpeaker(raw json.RawMessage) string {
	var n json.Number
	if json.Unmarshal(raw, &n) == nil && n != "" {
		return "speaker_" + n.String()
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || s == "" {
		return ""
	}
	if _, err := strconv.Atoi(s); err == nil {
		return "speaker_" + s
	}
	return s
}

func parseAzure(body []byte) (*transcript.Transcript, error) {
	var r azureResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("upstream returned invalid JSON: %w", err)
	}
	t := &transcript.Transcript{Duration: r.DurationMilliseconds / 1000, Segments: []transcript.Segment{}}
	var texts []string
	for _, c := range r.CombinedPhrases {
		texts = append(texts, strings.TrimSpace(c.Text))
	}
	end := 0.0
	for i, p := range r.Phrases {
		if i == 0 {
			t.Language = p.Locale
		}
		if len(r.CombinedPhrases) == 0 {
			texts = append(texts, strings.TrimSpace(p.Text))
		}
		sp := azureSpeaker(p.Speaker)
		seg := transcript.Segment{ID: len(t.Segments), Start: p.OffsetMilliseconds / 1000,
			End: (p.OffsetMilliseconds + p.DurationMilliseconds) / 1000, Text: strings.TrimSpace(p.Text), Speaker: sp}
		t.Segments = append(t.Segments, seg)
		end = max(end, seg.End)
		for _, w := range p.Words {
			t.Words = append(t.Words, transcript.Word{Word: w.Text, Start: w.OffsetMilliseconds / 1000,
				End: (w.OffsetMilliseconds + w.DurationMilliseconds) / 1000, Speaker: sp})
		}
	}
	t.Text = strings.TrimSpace(strings.Join(texts, " "))
	if t.Duration == 0 {
		t.Duration = end
	}
	return t, nil
}

// Audio formats that DetectAudio reports.
const (
	FormatWAV  = "wav"
	FormatFLAC = "flac"
	FormatMP3  = "mp3"
)

// AzureFormats are the upload formats that MAI-Transcribe takes.
var AzureFormats = map[string]string{FormatWAV: "audio/wav", FormatFLAC: "audio/flac", FormatMP3: "audio/mpeg"}

// DetectAudio names the container of an upload from its first bytes:
// wav, flac, mp3, ogg, webm, mp4, or "" when it is not known.
func DetectAudio(head []byte) string {
	switch {
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WAVE":
		return FormatWAV
	case bytes.HasPrefix(head, []byte("fLaC")):
		return FormatFLAC
	// shortcut: an ID3 tag in front of FLAC or AAC also reads as mp3; skip the tag if that shows up.
	case bytes.HasPrefix(head, []byte("ID3")):
		return FormatMP3
	case len(head) >= 3 && head[0] == 0xFF && head[1]&0xE0 == 0xE0 &&
		(head[1]>>3)&3 != 1 && (head[1]>>1)&3 != 0 && head[2]>>4 != 0xF:
		// MPEG frame sync with a valid version, a layer (ADTS AAC has layer 0) and bitrate index.
		return FormatMP3
	case bytes.HasPrefix(head, []byte("OggS")):
		return "ogg"
	case bytes.HasPrefix(head, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return "webm"
	case len(head) >= 8 && string(head[4:8]) == "ftyp":
		return "mp4"
	}
	return ""
}
