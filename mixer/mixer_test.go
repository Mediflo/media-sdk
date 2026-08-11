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
	"testing"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/stretchr/testify/require"
)

const (
	inputBufferMin = DefaultInputBufferFrames/2 + 1
)

func newTestWriter(buf *msdk.PCM16Sample, sampleRate int) msdk.PCM16Writer {
	return &testWriter{
		buf:        buf,
		sampleRate: sampleRate,
	}
}

type testWriter struct {
	buf        *msdk.PCM16Sample
	sampleRate int
}

func (b *testWriter) String() string {
	return fmt.Sprintf("testWriter(%d)", b.sampleRate)
}

func (b *testWriter) SampleRate() int {
	return b.sampleRate
}

func (b *testWriter) Close() error {
	return nil
}

func (b *testWriter) WriteSample(data msdk.PCM16Sample) error {
	*b.buf = data
	return nil
}

type testMixer struct {
	t      testing.TB
	sample msdk.PCM16Sample
	*Mixer
}

func newTestMixer(t testing.TB) *testMixer {
	m := &testMixer{t: t}

	m.Mixer = newMixer(newTestWriter(&m.sample, 8000), 5)
	return m
}

func (m *testMixer) Expect(exp msdk.PCM16Sample, msgAndArgs ...any) {
	m.t.Helper()
	m.mixOnce()
	require.Equal(m.t, exp, m.sample, msgAndArgs...)
}

func WriteSampleN(inp *Input, i int) {
	v := int16(i) * 5
	inp.WriteSample(msdk.PCM16Sample{v + 0, v + 1, v + 2, v + 3, v + 4})
}

func (m *testMixer) ExpectSampleN(i int, msgAndArgs ...any) {
	m.t.Helper()
	m.mixOnce()
	m.CheckSampleN(i, msgAndArgs...)
}

func (m *testMixer) CheckSampleN(i int, msgAndArgs ...any) {
	m.t.Helper()
	v := int16(i) * 5
	require.Equal(m.t, msdk.PCM16Sample{v + 0, v + 1, v + 2, v + 3, v + 4}, m.sample, msgAndArgs...)
}

func TestMixer(t *testing.T) {
	t.Run("no input produces silence", func(t *testing.T) {
		m := newTestMixer(t)
		m.mixOnce()
		m.Expect(msdk.PCM16Sample{0, 0, 0, 0, 0})
	})

	t.Run("one input mixing correctly", func(t *testing.T) {
		m := newTestMixer(t)
		inp := m.NewInput()
		defer inp.Close()
		inp.buffering = false

		WriteSampleN(inp, 1)
		m.ExpectSampleN(1)
	})

	t.Run("two inputs mixing correctly", func(t *testing.T) {
		m := newTestMixer(t)
		one := m.NewInput()
		defer one.Close()
		one.buffering = false
		one.WriteSample([]int16{0xE, 0xD, 0xC, 0xB, 0xA})

		two := m.NewInput()
		defer two.Close()
		two.buffering = false
		two.WriteSample([]int16{0xA, 0xB, 0xC, 0xD, 0xE})

		m.Expect(msdk.PCM16Sample{24, 24, 24, 24, 24})

		one.WriteSample([]int16{0x7FFF, 0x1, -0x7FFF, -0x1, 0x0})
		two.WriteSample([]int16{0x1, 0x7FFF, -0x1, -0x7FFF, 0x0})

		m.Expect(msdk.PCM16Sample{0x7FFF, 0x7FFF, -0x7FFF, -0x7FFF, 0x0})
	})

	t.Run("draining produces silence afterwards", func(t *testing.T) {
		m := newTestMixer(t)
		inp := m.NewInput()
		defer inp.Close()

		for i := 0; i < DefaultInputBufferFrames; i++ {
			inp.WriteSample([]int16{0, 1, 2, 3, 4})
		}

		for i := 0; i < DefaultInputBufferFrames+3; i++ {
			expected := msdk.PCM16Sample{0, 1, 2, 3, 4}
			if i >= DefaultInputBufferFrames {
				expected = msdk.PCM16Sample{0, 0, 0, 0, 0}
			}
			m.Expect(expected, "i=%d", i)
		}
	})

	t.Run("drops frames on overflow", func(t *testing.T) {
		m := newTestMixer(t)
		input := m.NewInput()
		defer input.Close()

		for i := 0; i < DefaultInputBufferFrames+3; i++ {
			input.WriteSample([]int16{0, 1, 2, 3, 4})
		}

		m.mixOnce()
		require.Equal(t, (DefaultInputBufferFrames-1)*5, input.buf.Len())
	})

	t.Run("buffered initially and after starving", func(t *testing.T) {
		m := newTestMixer(t)
		inp := m.NewInput()
		defer inp.Close()

		inp.WriteSample([]int16{10, 11, 12, 13, 14})

		for i := 0; i < inputBufferMin-1; i++ {
			// Mixing produces nothing, because we are buffering the input initially.
			m.Expect(msdk.PCM16Sample{0, 0, 0, 0, 0})
			WriteSampleN(inp, i)
		}

		// Now we should finally receive all our samples, even if no new data is available.
		m.Expect(msdk.PCM16Sample{10, 11, 12, 13, 14})
		for i := 0; i < inputBufferMin-1; i++ {
			m.ExpectSampleN(i)
		}

		// Input is starving, should produce silence again until we buffer enough samples.
		m.Expect(msdk.PCM16Sample{0, 0, 0, 0, 0})
		for i := 0; i < inputBufferMin; i++ {
			m.Expect(msdk.PCM16Sample{0, 0, 0, 0, 0})
			WriteSampleN(inp, i)
		}
		// Data is flowing again after we buffered enough.
		for i := 0; i < inputBufferMin; i++ {
			m.ExpectSampleN(i)
			// Keep writing to see if we can get this data later without gaps.
			WriteSampleN(inp, i*10)
		}
		// Check data that we were writing above.
		for i := 0; i < inputBufferMin; i++ {
			m.ExpectSampleN(i * 10)
		}
	})

	t.Run("catches up after not running for long", func(t *testing.T) {
		step := 20 * time.Millisecond
		m := newTestMixer(t)
		m.tickerDur = step

		inp := m.NewInput()
		defer inp.Close()

		for i := 0; i < DefaultInputBufferFrames; i++ {
			WriteSampleN(inp, i)
		}

		time.Sleep(step)
		m.mixUpdate()
		require.EqualValues(t, 1, m.mixCnt)
		m.CheckSampleN(0)

		const steps = DefaultInputBufferFrames/2 + 1
		time.Sleep(step*steps + step/2)
		m.mixUpdate()
		require.EqualValues(t, 1+steps, m.mixCnt)
		m.CheckSampleN(steps)
	})
}

func TestInputBufferMinFrames(t *testing.T) {
	t.Run("inherits the mixer default", func(t *testing.T) {
		m := newTestMixer(t)
		require.Equal(t, DefaultInputBufferMin, m.InputBufferMinFrames())
		require.Equal(t, 3, m.InputBufferMinFrames()) // 3 frames == 60ms at 20ms/frame

		inp := m.NewInput()
		defer inp.Close()
		require.Equal(t, DefaultInputBufferMin, inp.BufferMinFrames())

		// One frame is NOT enough to start playing at the inherited depth.
		inp.WriteSample([]int16{10, 11, 12, 13, 14})
		m.Expect(msdk.PCM16Sample{0, 0, 0, 0, 0})
	})

	t.Run("depth 1 starts playing on the first frame", func(t *testing.T) {
		m := newTestMixer(t)
		inp := m.NewInput()
		defer inp.Close()
		inp.SetBufferMinFrames(1)
		require.Equal(t, 1, inp.BufferMinFrames())

		inp.WriteSample([]int16{10, 11, 12, 13, 14})
		m.Expect(msdk.PCM16Sample{10, 11, 12, 13, 14})

		// And it keeps flowing frame-by-frame with only one frame in hand.
		for i := 0; i < 4; i++ {
			WriteSampleN(inp, i)
			m.ExpectSampleN(i, "i=%d", i)
		}
	})

	// The point of making this per-input: one source may run short while another
	// keeps the full smoothing, in the SAME mixer. sip-bridge relies on it to give
	// a live conversation the shorter path while platform prompts/hold music keep
	// the default depth.
	t.Run("depths are independent within one mixer", func(t *testing.T) {
		m := newTestMixer(t)
		shallow := m.NewInput()
		defer shallow.Close()
		shallow.SetBufferMinFrames(1)
		deep := m.NewInput()
		defer deep.Close()
		require.Equal(t, DefaultInputBufferMin, deep.BufferMinFrames())

		shallow.WriteSample([]int16{10, 11, 12, 13, 14})
		deep.WriteSample([]int16{1, 1, 1, 1, 1})
		// One frame is enough for the shallow input and not for the deep one.
		m.Expect(msdk.PCM16Sample{10, 11, 12, 13, 14})
		require.False(t, shallow.buffering)
		require.True(t, deep.buffering)

		// Nothing new arrives: the shallow input starves (that is the price of
		// depth 1), the deep one is still filling up.
		m.Expect(msdk.PCM16Sample{0, 0, 0, 0, 0})
		require.True(t, shallow.buffering)
		require.True(t, deep.buffering)

		// Bring the deep input up to its own 3-frame depth and hand the shallow one
		// a single frame; now both contribute to the same mix.
		deep.WriteSample([]int16{1, 1, 1, 1, 1})
		deep.WriteSample([]int16{1, 1, 1, 1, 1})
		shallow.WriteSample([]int16{10, 10, 10, 10, 10})
		m.Expect(msdk.PCM16Sample{11, 11, 11, 11, 11})
		require.False(t, shallow.buffering)
		require.False(t, deep.buffering)
	})

	t.Run("depth 1 does not shrink the input ring", func(t *testing.T) {
		// The overflow point is set by inputBufferFrames, which this setter must not
		// touch: WithInputBufferFrames(1) would leave room for a single frame and drop
		// on every burst. Same expectation as TestMixer/"drops frames on overflow".
		m := newTestMixer(t)
		input := m.NewInput()
		defer input.Close()
		input.SetBufferMinFrames(1)

		for i := 0; i < DefaultInputBufferFrames+3; i++ {
			input.WriteSample([]int16{0, 1, 2, 3, 4})
		}
		m.mixOnce()
		require.Equal(t, (DefaultInputBufferFrames-1)*5, input.buf.Len())
		require.EqualValues(t, DefaultInputBufferFrames, m.inputBufferFrames)
	})

	t.Run("reconfigurable under a live buffering input", func(t *testing.T) {
		// Mirrors production: the input is created by a track-subscribed callback and
		// its depth is set right after, while the mixer is already ticking.
		m := newTestMixer(t)
		inp := m.NewInput()
		defer inp.Close()

		inp.WriteSample([]int16{10, 11, 12, 13, 14})
		m.Expect(msdk.PCM16Sample{0, 0, 0, 0, 0}) // buffering at the inherited depth 3
		require.True(t, inp.buffering)

		inp.SetBufferMinFrames(1)
		// The already-buffered frame is still there, and plays now — nothing lost.
		m.Expect(msdk.PCM16Sample{10, 11, 12, 13, 14})
		require.False(t, inp.buffering)
	})

	t.Run("clamped and reset", func(t *testing.T) {
		m := newTestMixer(t)
		inp := m.NewInput()
		defer inp.Close()
		inp.SetBufferMinFrames(DefaultInputBufferFrames + 10)
		require.Equal(t, DefaultInputBufferFrames, inp.BufferMinFrames())
		inp.SetBufferMinFrames(0)
		require.Equal(t, DefaultInputBufferMin, inp.BufferMinFrames())
		inp.SetBufferMinFrames(1)
		inp.SetBufferMinFrames(-1)
		require.Equal(t, DefaultInputBufferMin, inp.BufferMinFrames())
	})

	t.Run("clamp and reset follow a custom inputBufferFrames", func(t *testing.T) {
		m := newMixer(newTestWriter(new(msdk.PCM16Sample), 8000), 5, WithInputBufferFrames(9))
		require.Equal(t, 5, m.InputBufferMinFrames())
		inp := m.NewInput()
		defer inp.Close()
		require.Equal(t, 5, inp.BufferMinFrames())
		inp.SetBufferMinFrames(1)
		require.Equal(t, 1, inp.BufferMinFrames())
		inp.SetBufferMinFrames(100)
		require.Equal(t, 9, inp.BufferMinFrames())
		inp.SetBufferMinFrames(0)
		require.Equal(t, 5, inp.BufferMinFrames())
	})

	t.Run("nil input is a no-op", func(t *testing.T) {
		var inp *Input
		inp.SetBufferMinFrames(1)
		require.Equal(t, 0, inp.BufferMinFrames())
	})
}
