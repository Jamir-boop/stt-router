package server

import (
	"encoding/binary"
	"math"
	"testing"
)

// pcmTone returns ms of a sine wave at amp (0..1) and rate Hz.
func pcmTone(rate, hz, ms int, amp float64) []byte {
	n := rate * ms / 1000
	b := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := amp * math.Sin(2*math.Pi*float64(hz)*float64(i)/float64(rate))
		binary.LittleEndian.PutUint16(b[i*2:], uint16(int16(v*32767)))
	}
	return b
}

func pcmSilence(rate, ms int) []byte { return make([]byte, rate*ms/1000*2) }

// runVAD feeds 20 ms pieces and stamps each event with the audio time fed so far.
func runVAD(v *vad, pcm []byte) (starts, stops []int, turns []*vadTurn) {
	n := v.frameBytes()
	ms := 0
	for len(pcm) > 0 {
		k := min(n, len(pcm))
		for _, ev := range v.feed(pcm[:k]) {
			if ev.started {
				starts = append(starts, ms)
			}
			if ev.stopped {
				stops = append(stops, ms)
				if ev.turn != nil {
					turns = append(turns, ev.turn)
				}
			}
		}
		pcm = pcm[k:]
		ms += k / 2 * 1000 / v.cfg.rate
	}
	return
}

func TestVADToneBurstOneTurn(t *testing.T) {
	rate := 16000
	v := newVAD(vadConfig{rate: rate, threshold: 0.6, silenceMs: 600, prefixMs: 500})
	var pcm []byte
	pcm = append(pcm, pcmSilence(rate, 1000)...)
	pcm = append(pcm, pcmTone(rate, 440, 1000, 0.5)...)
	pcm = append(pcm, pcmSilence(rate, 1000)...)
	starts, stops, turns := runVAD(v, pcm)
	if len(starts) != 1 || len(stops) != 1 || len(turns) != 1 {
		t.Fatalf("starts %v stops %v turns %d", starts, stops, len(turns))
	}
	// Speech begins at 1000 ms; the start event comes 100 ms later, and it keeps 500 ms of audio before the onset.
	if starts[0] < 1000 || starts[0] > 1150 {
		t.Errorf("speech_started at %d ms", starts[0])
	}
	// The tone ends at 2000 ms; speech_stopped comes 600 ms of silence later.
	if stops[0] < 2500 || stops[0] > 2750 {
		t.Errorf("speech_stopped at %d ms", stops[0])
	}
	// 500 ms of padding, the whole 1 s tone, and the 600 ms of trailing silence.
	want := (500 + 1000 + 600) * rate / 1000 * 2
	if d := len(turns[0].pcm) - want; d < -2*vadFrameMs*rate/1000*2 || d > 2*vadFrameMs*rate/1000*2 {
		t.Errorf("turn audio %d bytes, want about %d", len(turns[0].pcm), want)
	}
}

func TestVADClickIsNoTurn(t *testing.T) {
	rate := 24000
	v := newVAD(vadConfig{rate: rate, threshold: 0.6, silenceMs: 600, prefixMs: 500})
	var pcm []byte
	pcm = append(pcm, pcmSilence(rate, 500)...)
	pcm = append(pcm, pcmTone(rate, 440, 100, 0.5)...)
	pcm = append(pcm, pcmSilence(rate, 1000)...)
	_, _, turns := runVAD(v, pcm)
	if len(turns) != 0 {
		t.Fatalf("a 100 ms click must give no turn, got %d", len(turns))
	}
}

func TestVADForceCut(t *testing.T) {
	rate := 16000
	v := newVAD(vadConfig{rate: rate, threshold: 0.6, silenceMs: 600, prefixMs: 500})
	pcm := pcmTone(rate, 440, 20000, 0.5)
	starts, stops, turns := runVAD(v, pcm)
	if len(starts) != 2 || len(stops) != 1 || len(turns) != 1 {
		t.Fatalf("20 s of tone: starts %v stops %v turns %d", starts, stops, len(turns))
	}
	// The first turn is cut at exactly 15 s of buffered audio, and the tone
	// after it goes into the next turn.
	if got, want := len(turns[0].pcm), 15*rate*2; got != want {
		t.Errorf("first turn %d bytes, want %d", got, want)
	}
	if stops[0] < 14900 || stops[0] > 15100 {
		t.Errorf("force cut at %d ms, want about 15000", stops[0])
	}
}

func TestVADThresholdMovesSensitivity(t *testing.T) {
	rate := 16000
	// -49 dBFS RMS, which is between the threshold 0 level and the threshold 1 level.
	pcm := make([]byte, 200*rate/1000*2)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], uint16(int16(227*math.Sin(2*math.Pi*440*float64(i/2)/float64(rate)))))
	}
	quiet := func(threshold float64) bool {
		v := newVAD(vadConfig{rate: rate, threshold: threshold, silenceMs: 600, prefixMs: 500})
		starts := 0
		for _, ev := range v.feed(pcm) {
			if ev.started {
				starts++
			}
		}
		return starts > 0
	}
	if !quiet(0) || quiet(1) {
		t.Errorf("a low threshold must hear a quiet tone and a high threshold must not")
	}
}
