// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mixer

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frostbyte73/core"
	msdk "github.com/livekit/media-sdk"

	"github.com/livekit/media-sdk/ring"
)

const (
	DefaultInputBufferFrames = 5
	DefaultInputBufferMin    = DefaultInputBufferFrames/2 + 1
)

type Stats struct {
	Tracks       atomic.Int64
	TracksTotal  atomic.Uint64
	Restarts     atomic.Uint64
	TimingResets atomic.Uint64

	Mixes         atomic.Uint64
	TimedMixes    atomic.Uint64
	JumpMixes     atomic.Uint64
	ZeroMixes     atomic.Uint64
	NegativeMixes atomic.Uint64

	InputSamples        atomic.Uint64
	InputFrames         atomic.Uint64
	InputSamplesDropped atomic.Uint64
	InputFramesDropped  atomic.Uint64

	MixedSamples atomic.Uint64
	MixedFrames  atomic.Uint64

	OutputSamples atomic.Uint64
	OutputFrames  atomic.Uint64

	WriteErrors  atomic.Uint64
	BlockedMixes atomic.Uint64
}

type Input struct {
	m          *Mixer
	sampleRate int
	mu         sync.Mutex
	buf        *ring.Buffer[int16]
	buffering  bool
	// bufMinFrames is this input's minimum buffering depth in frames, seeded from
	// the mixer's inputBufferMin at NewInput. Per-input so a caller can trade
	// latency for smoothing on one source without touching the others — see
	// SetBufferMinFrames.
	bufMinFrames int
}

type Mixer struct {
	out        msdk.Writer[msdk.PCM16Sample]
	outchan    chan msdk.PCM16Sample // Write mixed frames to this channel, write to out directly if nil
	sampleRate int

	mu     sync.Mutex
	inputs []*Input

	tickerDur time.Duration
	ticker    *time.Ticker
	mixBuf    []int32          // mix result buffer
	mixTmp    msdk.PCM16Sample // temp buffer for reading input buffers

	lastMixEndTs time.Time
	stopped      core.Fuse
	mixCnt       uint

	// inputBufferFrames sets max number of frames that each mixer input will allow.
	// Sending more frames to the input will cause old one to be dropped.
	inputBufferFrames int
	// inputBufferMin is the minimal number of buffered frames required to start mixing.
	// It affects inputs initially, or after they start to starve.
	inputBufferMin int

	stats *Stats
}

type MixerOptions func(*Mixer)

func WithOutputChannel() MixerOptions {
	// This still uses a channel, but it's 1-deep, and would block if the downstream goroutine is blocked.
	// This preserves the effects of not using a channel at all.
	return WithOutputChannelSize(1)
}

// Makes mixer write to a channel in place of directly calling out.WriteSample(), which unblocks the mixer ticker.
func WithOutputChannelSize(size int) MixerOptions {
	return func(m *Mixer) {
		if size <= 0 {
			size = 1
		}
		m.outchan = make(chan msdk.PCM16Sample, size)
	}
}

func WithInputBufferFrames(frames int) MixerOptions {
	return func(m *Mixer) {
		if frames <= 0 {
			frames = DefaultInputBufferFrames
		}
		m.inputBufferFrames = frames
		m.inputBufferMin = frames/2 + 1
	}
}

func WithStats(stats *Stats) MixerOptions {
	return func(m *Mixer) {
		m.stats = stats
	}
}

func NewMixer(out msdk.Writer[msdk.PCM16Sample], bufferDur time.Duration, channels int, options ...MixerOptions) (*Mixer, error) {
	if channels != 1 {
		return nil, fmt.Errorf("only mono mixing is supported")
	}

	mixSize := int(time.Duration(out.SampleRate()) * bufferDur / time.Second)
	m := newMixer(out, mixSize, options...)
	m.tickerDur = bufferDur
	m.ticker = time.NewTicker(bufferDur)

	go m.start()

	return m, nil
}

func newMixer(out msdk.Writer[msdk.PCM16Sample], mixSize int, options ...MixerOptions) *Mixer {
	m := &Mixer{
		out:               out,
		outchan:           nil, // Write directly to out
		sampleRate:        out.SampleRate(),
		mixBuf:            make([]int32, mixSize),
		mixTmp:            make(msdk.PCM16Sample, mixSize),
		stats:             nil,
		inputBufferFrames: DefaultInputBufferFrames,
		inputBufferMin:    DefaultInputBufferMin,
	}
	for _, option := range options {
		option(m)
	}
	if m.stats == nil {
		m.stats = new(Stats)
	}
	return m
}

func (m *Mixer) mixInputs() {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Each input keeps at least its own bufMinFrames frames buffered (seeded from
	// inputBufferMin, which by default keeps at least half of the samples buffered).
	frame := len(m.mixBuf)
	for _, inp := range m.inputs {
		n, _ := inp.readSample(frame, m.mixTmp[:frame])
		if n == 0 {
			continue
		}

		m.stats.MixedFrames.Add(1)
		m.stats.MixedSamples.Add(uint64(n))

		m.mixTmp = m.mixTmp[:n]
		for j, v := range m.mixTmp {
			// Add the samples. This can potentially lead to overflow, but is unlikely and dividing by the source
			// count would cause the volume to drop every time somebody joins
			m.mixBuf[j] += int32(v)
		}
	}
}

func (m *Mixer) reset() {
	for i := range m.mixBuf {
		m.mixBuf[i] = 0
	}
}

func (m *Mixer) mixOnce() {
	m.stats.Mixes.Add(1)
	m.mixCnt++
	m.reset()
	m.mixInputs()

	out := make(msdk.PCM16Sample, len(m.mixBuf)) // Can be buffered by either channel or m.out
	for i, v := range m.mixBuf {
		if v > 0x7FFF {
			v = 0x7FFF
		}
		if v < -0x7FFF {
			v = -0x7FFF
		}
		out[i] = int16(v)
	}

	m.stats.OutputFrames.Add(1)
	m.stats.OutputSamples.Add(uint64(len(out)))

	if m.outchan == nil {
		err := m.out.WriteSample(out)
		if err != nil {
			m.stats.WriteErrors.Add(1)
		}
		return
	} else {
		select {
		case m.outchan <- out: // Try to push without blocking
		default:
			// Blocked, wait for output channel to be ready
			m.stats.BlockedMixes.Add(1)
			// Blocking, mimics behavior witohut channel
			// TODO: Consider, carefully, dropping when blocked
			m.outchan <- out
		}
	}
}

func (m *Mixer) mixUpdate() {
	n := 0
	now := time.Now()

	if m.lastMixEndTs.IsZero() {
		m.stats.TimedMixes.Add(1)
		m.lastMixEndTs = now
		n = 1
	} else {
		dt := now.Sub(m.lastMixEndTs)
		if dt < 0 {
			// Can happen when last time we went a little over due to fuzz. Nothing to do.
			m.stats.NegativeMixes.Add(1)
			return
		}
		// In case scheduler stops us for too long, we will detect it and run mix multiple times.
		// This happens if we get scheduled by OS/K8S on a lot of CPUs, but for a very short time.
		dt += m.tickerDur / 4 // Add fuzz to account for wake-up jitter after negative check
		n = int(dt / m.tickerDur)
		m.lastMixEndTs = m.lastMixEndTs.Add(time.Duration(n) * m.tickerDur)
		switch n {
		case 0: // Baseline lastMixEndTs got set later than necessary
			m.stats.ZeroMixes.Add(1)
		case 1: // All is well
			m.stats.TimedMixes.Add(1)
		default: // We've not woken up in quite some time, count the skipped mixes as jumps
			m.stats.JumpMixes.Add(uint64(n))
		}
	}
	if n > m.inputBufferFrames {
		n = m.inputBufferFrames
		m.stats.TimingResets.Add(uint64(n))
		m.lastMixEndTs = now
	}
	for i := 0; i < n; i++ {
		m.mixOnce()
	}
}

func (m *Mixer) writer() {
	for {
		select {
		case mixed := <-m.outchan:
			err := m.out.WriteSample(mixed)
			if err != nil {
				m.stats.WriteErrors.Add(1)
			}
		case <-m.stopped.Watch():
			return
		}
	}
}

func (m *Mixer) start() {
	if m.outchan != nil {
		go m.writer()
	}
	defer m.ticker.Stop()
	for {
		select {
		case <-m.ticker.C:
			m.mixUpdate()
		case <-m.stopped.Watch():
			return
		}
	}
}

func (m *Mixer) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped.Break()
}

func (m *Mixer) NewInput() *Input {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped.IsBroken() {
		return nil
	}

	m.stats.Tracks.Add(1)
	m.stats.TracksTotal.Add(1)

	inp := &Input{
		m:            m,
		sampleRate:   m.sampleRate,
		buf:          ring.NewBuffer[int16](len(m.mixBuf) * m.inputBufferFrames),
		buffering:    true, // buffer some data initially
		bufMinFrames: m.inputBufferMin,
	}
	m.inputs = append(m.inputs, inp)
	return inp
}

func (m *Mixer) RemoveInput(inp *Input) {
	if m == nil || inp == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	i := slices.Index(m.inputs, inp)
	if i < 0 {
		return
	}
	m.inputs = slices.Delete(m.inputs, i, i+1)
	m.stats.Tracks.Add(-1)
}

// InputBufferMinFrames reports the minimum input buffering depth, in frames, that
// new inputs inherit (see Input.SetBufferMinFrames to change one input's depth).
func (m *Mixer) InputBufferMinFrames() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inputBufferMin
}

// maxInputBufferFrames reports the input ring capacity in frames — the ceiling for
// any input's buffering depth, since a deeper minimum could never be reached.
func (m *Mixer) maxInputBufferFrames() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inputBufferFrames
}

func (m *Mixer) String() string {
	return fmt.Sprintf("Mixer(%d) -> %s", len(m.inputs), m.out.String())
}

func (m *Mixer) SampleRate() int {
	return m.sampleRate
}

// readSample reads up to one frame. frameSize is the mixer's frame length in
// samples; the input's own depth (bufMinFrames) scales it into the number of
// buffered samples required before playback (re)starts.
func (i *Input) readSample(frameSize int, out msdk.PCM16Sample) (int, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.buffering {
		if i.buf.Len() < i.bufMinFrames*frameSize {
			return 0, nil // keep buffering
		}
		// buffered enough data - start playing as usual
		i.buffering = false
	}
	n, err := i.buf.Read(out)
	if n == 0 {
		i.buffering = true // starving; pause the input and start buffering again
		i.m.stats.Restarts.Add(1)
	}
	return n, err
}

// SetBufferMinFrames changes how many frames THIS input must have buffered before
// the mixer starts (or resumes) reading it. It is the knob that decides the
// input's steady-state latency: an input settles at roughly this much buffered
// audio, so on the 20 ms frames a SIP leg uses the inherited default of 3 frames
// is ~60 ms of added mouth-to-ear delay, and 1 frame is ~20 ms. In exchange the
// input starves (and re-buffers, counting Stats.Restarts) on jitter the removed
// slack would have absorbed — which is why this is per-input: a caller can shorten
// a live conversation's path while leaving, say, a prompt/music source smooth.
//
// Unlike WithInputBufferFrames this does NOT resize anything: inputBufferFrames
// keeps sizing the ring (burst tolerance / overflow point) and keeps capping the
// catch-up mixes in mixUpdate. Lowering only the minimum therefore buys latency
// without making the input drop frames on a burst or wedging the catch-up path —
// WithInputBufferFrames(1) would do both, since it shrinks every ring to a single
// frame and caps catch-up at one mix.
//
// Safe to call at any time, including while the mixer is running and this input is
// already receiving audio: bufMinFrames is a pure threshold, read only under i.mu
// in readSample, and never used to size or index anything. It cannot drop,
// duplicate or reorder a sample; the only observable effect is that an input which
// happens to be (re)buffering right now starts playing after fewer frames.
//
// frames <= 0 restores the mixer's inherited default. Larger values are clamped to
// the ring's capacity in frames, because an input can never buffer more than its
// ring holds and an unclamped value would leave it buffering forever.
func (i *Input) SetBufferMinFrames(frames int) {
	if i == nil {
		return
	}
	// Both read under m.mu and released before i.mu is taken, so this never nests
	// the two locks (mixInputs already holds m.mu while readSample takes i.mu).
	def, maxFrames := i.m.InputBufferMinFrames(), i.m.maxInputBufferFrames()
	i.mu.Lock()
	defer i.mu.Unlock()
	if frames <= 0 {
		frames = def
	}
	if frames > maxFrames {
		frames = maxFrames
	}
	i.bufMinFrames = frames
}

// BufferMinFrames reports this input's minimum buffering depth, in frames.
func (i *Input) BufferMinFrames() int {
	if i == nil {
		return 0
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.bufMinFrames
}

func (i *Input) String() string {
	return fmt.Sprintf("MixInput(%d) -> %s", i.sampleRate, i.m.String())
}

func (i *Input) SampleRate() int {
	return i.sampleRate
}

func (i *Input) Close() error {
	if i == nil {
		return nil
	}
	i.m.RemoveInput(i)
	return nil
}

func (i *Input) WriteSample(sample msdk.PCM16Sample) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.m.stats.InputFrames.Add(1)
	i.m.stats.InputSamples.Add(uint64(len(sample)))
	if discarded := i.buf.Len() + len(sample) - i.buf.Size(); discarded > 0 {
		i.m.stats.InputFramesDropped.Add(1)
		i.m.stats.InputSamplesDropped.Add(uint64(discarded))
	}

	_, err := i.buf.Write(sample)
	return err
}
