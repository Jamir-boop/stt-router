package server

// An energy voice activity detector for 16-bit mono PCM. It holds only state
// and a feed method, so tests can drive it with synthetic audio.

import (
	"encoding/binary"
	"math"
)

const (
	vadFrameMs     = 20
	vadOnsetFrames = 5      // 100 ms of speech frames starts a turn
	vadMinSpeechMs = 300    // shorter turns are not transcribed
	vadMaxTurnMs   = 15000  // a turn is force-cut at 15 s
	vadFloor       = 0.0056 // about -45 dBFS
	vadNoiseAlpha  = 0.02
)

type vadConfig struct {
	rate      int
	threshold float64 // client sensitivity 0..1; 0.6 is neutral
	silenceMs int
	prefixMs  int
}

type vadTurn struct {
	pcm      []byte
	speechMs int
}

// vadEvent marks a turn start or end. turn is set on a stop that carries
// enough speech to transcribe.
type vadEvent struct {
	started bool
	stopped bool
	turn    *vadTurn
}

type vad struct {
	cfg    vadConfig
	rest   []byte   // a partial frame between feed calls
	hist   [][]byte // recent frames kept as prefix padding
	noise  float64  // slowly adapting noise floor, in RMS
	onset  int      // consecutive speech frames outside a turn
	in     bool
	silent int // consecutive silence ms inside a turn
	cur    *vadTurn
}

func newVAD(cfg vadConfig) *vad { return &vad{cfg: cfg, noise: 1e-4} }

func (v *vad) frameBytes() int { return v.cfg.rate / 50 * 2 }

// level is the RMS above which a frame counts as speech.
func (v *vad) level() float64 {
	return max(vadFloor*(1+(v.cfg.threshold-0.6)), 3*v.noise)
}

// isSpeech measures one frame and adapts the noise floor on non-speech.
func (v *vad) isSpeech(f []byte) bool {
	var sum float64
	for i := 0; i+1 < len(f); i += 2 {
		x := float64(int16(binary.LittleEndian.Uint16(f[i:]))) / 32768
		sum += x * x
	}
	rms := math.Sqrt(sum / float64(len(f)/2))
	if rms > v.level() {
		return true
	}
	v.noise = (1-vadNoiseAlpha)*v.noise + vadNoiseAlpha*rms
	return false
}

func (v *vad) pushHist(f []byte) {
	v.hist = append(v.hist, append([]byte(nil), f...))
	// A client can send any prefix_padding_ms; the buffer still stays bounded.
	keep := min(v.cfg.prefixMs/vadFrameMs+vadOnsetFrames, vadMaxTurnMs/vadFrameMs)
	if len(v.hist) > keep {
		v.hist = append([][]byte(nil), v.hist[len(v.hist)-keep:]...)
	}
}

// feed adds PCM bytes and returns the events they caused.
func (v *vad) feed(pcm []byte) []vadEvent {
	data := pcm
	if len(v.rest) > 0 {
		data = append(v.rest, pcm...)
		v.rest = nil
	}
	var evs []vadEvent
	n := v.frameBytes()
	for len(data) >= n {
		evs = append(evs, v.frame(data[:n])...)
		data = data[n:]
	}
	v.rest = append([]byte(nil), data...)
	return evs
}

func (v *vad) frame(f []byte) []vadEvent {
	sp := v.isSpeech(f)
	if !v.in {
		v.pushHist(f)
		if sp {
			v.onset++
		} else {
			v.onset = 0
		}
		if v.onset < vadOnsetFrames {
			return nil
		}
		v.in = true
		v.cur = &vadTurn{speechMs: v.onset * vadFrameMs}
		for _, h := range v.hist {
			v.cur.pcm = append(v.cur.pcm, h...)
		}
		v.hist = nil
		v.silent = 0
		return []vadEvent{{started: true}}
	}
	v.cur.pcm = append(v.cur.pcm, f...)
	if sp {
		v.cur.speechMs += vadFrameMs
		v.silent = 0
	} else {
		v.silent += vadFrameMs
	}
	if v.silent < v.cfg.silenceMs && len(v.cur.pcm) < v.cfg.rate*2*vadMaxTurnMs/1000 {
		return nil
	}
	return []vadEvent{v.stop()}
}

func (v *vad) stop() vadEvent {
	t := v.cur
	v.in, v.cur, v.silent, v.onset = false, nil, 0, 0
	if t.speechMs < vadMinSpeechMs {
		return vadEvent{stopped: true}
	}
	return vadEvent{stopped: true, turn: t}
}

// flush ends the current turn at an explicit commit. It returns nil when the
// turn is too short or there is none.
func (v *vad) flush() *vadTurn {
	if !v.in {
		return nil
	}
	t := v.cur
	v.in, v.cur, v.silent, v.onset = false, nil, 0, 0
	if t.speechMs < vadMinSpeechMs {
		return nil
	}
	return t
}
