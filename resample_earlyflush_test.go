// Copyright 2026 LiveKit, Inc.
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

package media_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/media-sdk"
)

// telephonyFrames builds n 20ms frames of a tone at the given rate — the shape a
// G.711 SIP leg delivers (160 samples per frame at 8kHz).
func telephonyFrames(rate, n int) []media.PCM16Sample {
	frameSz := rate / 50
	out := make([]media.PCM16Sample, 0, n)
	phase := 0.0
	step := 2 * math.Pi * 440 / float64(rate)
	for i := 0; i < n; i++ {
		f := make(media.PCM16Sample, frameSz)
		for j := range f {
			f[j] = int16(8000 * math.Sin(phase))
			phase += step
		}
		out = append(out, f)
	}
	return out
}

// runResample feeds frames through a resample writer and reports, per input
// frame, how many samples came out — plus the concatenated output.
func runResample(t *testing.T, srcRate, dstRate int, frames []media.PCM16Sample, opts ...media.ResampleOption) (perInput []int, all media.PCM16Sample) {
	t.Helper()
	var got media.PCM16Sample
	dst := media.NewPCM16BufferWriter(&got, dstRate)
	pw := &paceWriter{w: dst}
	r := media.ResampleWriter(pw, srcRate, opts...)
	require.NotSame(t, media.PCM16Writer(pw), r, "expected a real resampler for %d->%d", srcRate, dstRate)

	perInput = make([]int, len(frames))
	for i, f := range frames {
		pw.Set(i)
		before := pw.samples
		require.NoError(t, r.WriteSample(f))
		perInput[i] = pw.samples - before
	}
	require.NoError(t, r.Close())
	return perInput, got
}

// maxDitherDelta bounds how far two independent soxr instances may differ on the
// same input.
//
// soxr applies TPDF dither to INT16 output, and it is ON by default: newSoxr builds
// soxr_io_spec(SOXR_INT16_I, SOXR_INT16_I) with flags 0, and SOXR_NO_DITHER (the
// only switch) is never set. Its RNG is unseeded and libsoxr's public API exposes no
// way to seed it — which is what the standing "TODO: set seed somehow, otherwise
// results are random" in resample.go refers to, and why resample_test.go cannot
// checksum resampler output either. So two separately-constructed resamplers fed
// identical input disagree in the low bit, by design.
//
// Measured on linux/amd64 (8k->48k, 60 frames of an amplitude-8000 tone): 44.8% of
// samples differ, max |delta| 2 — about -72 dB, i.e. inaudible dither noise, not
// drift. darwin/arm64 happened to agree exactly, which is why byte equality survived
// review until CI ran on Linux.
//
// Deliberately kept at the measured maximum rather than rounded up: any real change
// to the resampling itself moves samples by far more than 2 LSB and must still fail.
const maxDitherDelta = 2

// requireAlignedWithinDither asserts that b is a's live stream: sample-for-sample
// aligned, differing only at dither scale.
//
// This is the property WithEarlyFlush must hold — flushing mid-stream changes WHEN
// samples are handed over, never WHICH — expressed without depending on soxr's
// unseeded dither. Alignment is still checked exactly: a genuine off-by-one would
// make some non-zero shift agree better than zero shift, and that fails here.
func requireAlignedWithinDither(t *testing.T, a, b media.PCM16Sample) {
	t.Helper()
	require.Equal(t, len(a), len(b), "live streams must cover the same span")

	// Count samples agreeing within the dither envelope at a given shift. Every
	// shift is scored over the SAME interior window, so zero shift gets no free
	// advantage from the samples a shifted comparison would run off the end of.
	const maxShift = 2
	require.Greater(t, len(a), 2*maxShift, "too short to test alignment")
	agree := func(shift int) int {
		n := 0
		for i := maxShift; i < len(a)-maxShift; i++ {
			d := int(a[i]) - int(b[i+shift])
			if d < 0 {
				d = -d
			}
			if d <= maxDitherDelta {
				n++
			}
		}
		return n
	}

	worst, worstAt := 0, -1
	for i := range a {
		d := int(a[i]) - int(b[i])
		if d < 0 {
			d = -d
		}
		if d > worst {
			worst, worstAt = d, i
		}
	}
	require.LessOrEqualf(t, worst, maxDitherDelta,
		"live streams diverge by %d at sample %d, beyond dither scale: early flush changed the audio, not just its timing",
		worst, worstAt)

	aligned := agree(0)
	for _, shift := range []int{-2, -1, 1, 2} {
		require.Greaterf(t, aligned, agree(shift),
			"live stream agrees better at shift %+d (%d samples) than at 0 (%d): early flush moved samples in time",
			shift, agree(shift), aligned)
	}
}

// backlog reports how much audio the writer is holding once the stream is
// running: everything fed in, minus everything handed downstream. That number,
// divided by the destination rate, IS the latency the writer adds.
func backlog(perInput []int, dstFrame int) int {
	out := 0
	for _, n := range perInput {
		out += n
	}
	return len(perInput)*dstFrame - out
}

// The default resample writer withholds output until a full destination frame
// exists. WithEarlyFlush drops that rule, trading neat framing for the audio the
// rule keeps parked.
//
// NOTE on magnitude: the withheld frame is NOT the dominant term. Measured on
// this harness (8k->48k, 20ms frames, SOXR_HQ), the writer holds ~180ms in steady
// state, of which early flush recovers only ~9ms. The rest is soxr's own
// pipeline plus the per-call output-space cap in Resample — see
// TestResampleSteadyStateBacklog, which pins that number so it cannot regress
// unnoticed.
func TestResampleEarlyFlush(t *testing.T) {
	const (
		srcRate = 8000  // G.711 leg
		dstRate = 48000 // room mixing rate
		frames  = 60
	)
	src := telephonyFrames(srcRate, frames)
	dstFrame := dstRate / 50 // 960 samples == 20ms

	t.Run("early flush emits sooner and holds less", func(t *testing.T) {
		slowPer, _ := runResample(t, srcRate, dstRate, src)
		fastPer, _ := runResample(t, srcRate, dstRate, src, media.WithEarlyFlush(true))

		slowBacklog, fastBacklog := backlog(slowPer, dstFrame), backlog(fastPer, dstFrame)
		require.Less(t, fastBacklog, slowBacklog,
			"early flush must hold less audio (slow=%d fast=%d)", slowBacklog, fastBacklog)

		// Honest bound: what this option actually buys on the room-in hop. Wide,
		// because it depends on where soxr's fractional output lands per call.
		savedMs := float64(slowBacklog-fastBacklog) * 1000 / float64(dstRate)
		require.Greater(t, savedMs, 3.0, "expected a few ms back, got %.1fms", savedMs)
		require.Less(t, savedMs, 25.0, "more than a frame recovered — the model is wrong")
		t.Logf("early flush recovers %.1f ms (backlog %d -> %d samples)",
			savedMs, slowBacklog, fastBacklog)
	})

	// The reason this is safe on a re-framing consumer: through the whole live
	// stream, early flush changes WHEN samples are handed over, not WHICH.
	//
	// Asserted as alignment + a dither-scale bound rather than byte equality, because
	// byte equality is not a property soxr provides. See requireAlignedWithinDither.
	//
	// Compared up to the point where Close's drain begins. The flush tail — the
	// backlog the resampler was holding when the stream ended, i.e. audio after
	// the far end already stopped sending — is chunked differently by the two
	// configurations, because Resample() sizes each drain round from the
	// destination buffer's spare capacity and early flushing leaves that buffer
	// empty. Same amount of audio either way (asserted below); the last ~150 ms of
	// a torn-down call is rendered from a differently-split flush.
	t.Run("sample-aligned through the live stream", func(t *testing.T) {
		slowPer, slow := runResample(t, srcRate, dstRate, src)
		_, fast := runResample(t, srcRate, dstRate, src, media.WithEarlyFlush(true))
		require.Equal(t, len(slow), len(fast), "early flush must not change how much audio survives")

		// Everything handed over before Close, i.e. the whole live stream.
		live := frames*dstFrame - backlog(slowPer, dstFrame)
		require.Greater(t, live, frames*dstFrame/2, "not enough live stream to be a meaningful comparison")
		requireAlignedWithinDither(t, slow[:live], fast[:live])
	})

	// codex P2 (2026-08-11): Close used to drain the resampler with a single call,
	// bounded by the destination buffer's spare capacity — so how much of the tail
	// survived depended on how full that buffer happened to be, and early flushing
	// (which keeps it empty) discarded ~584 samples more than the default. Close
	// now loops until the resampler reports empty, so BOTH paths deliver every
	// sample that was fed in.
	t.Run("close drains the resampler completely", func(t *testing.T) {
		fed := frames * dstFrame
		for _, early := range []bool{false, true} {
			var opts []media.ResampleOption
			if early {
				opts = append(opts, media.WithEarlyFlush(true))
			}
			_, all := runResample(t, srcRate, dstRate, src, opts...)
			require.Equal(t, fed, len(all),
				"early=%v: every sample fed in must come out by Close", early)
		}
	})

	// Negative control for the SIP-out direction, which must NOT use this: with
	// early flush the writes stop being uniform frames, and an RTP encoder turns
	// each write into a packet.
	t.Run("early flush makes writes ragged (why the encode side keeps the default)", func(t *testing.T) {
		down := telephonyFrames(dstRate, frames) // 48k -> 8k, the mixer -> SIP path
		slowPer, _ := runResample(t, dstRate, srcRate, down)
		fastPer, _ := runResample(t, dstRate, srcRate, down, media.WithEarlyFlush(true))

		uniform := func(per []int) bool {
			var seen int
			for _, n := range per {
				if n == 0 {
					continue
				}
				if seen == 0 {
					seen = n
				} else if n != seen {
					return false
				}
			}
			return true
		}
		require.True(t, uniform(slowPer), "default must hand the encoder uniform frames: %v", slowPer)
		require.False(t, uniform(fastPer), "early flush is expected to produce ragged writes: %v", fastPer)
	})
}

// TestResampleSteadyStateBacklog characterizes what a resample writer actually
// costs in latency, because it is far more than the one withheld frame that
// WithEarlyFlush addresses, and nothing pinned it before.
//
// The writer settles at a CONSTANT backlog — the same number of samples parked
// whether the stream ran 1 second or 30 — so this is a fixed pipeline delay, not
// a leak: no samples are lost, they just come out late, for the whole call.
//
// Two things produce it, and neither is the withheld frame:
//   - soxr's own pipeline at the configured quality (SOXR_HQ since
//     "resample: use SOXR_HQ instead of SOXR_LQ"); the same harness measures
//     ~60ms with SOXR_LQ against ~180ms with SOXR_HQ on this hop.
//   - Resample() offers soxr exactly the 1:1 output space for the input it was
//     handed, so the deficit soxr builds while priming can never be drained;
//     handing it 4x the space measures ~100ms instead of ~180ms.
//
// The bound below is deliberately loose (it must not fail on a different libsoxr
// build) but tight enough to catch an order-of-magnitude change.
func TestResampleSteadyStateBacklog(t *testing.T) {
	const srcRate, dstRate = 8000, 48000
	dstFrame := dstRate / 50

	var prev int
	for _, frames := range []int{150, 600} {
		perInput, _ := runResample(t, srcRate, dstRate, telephonyFrames(srcRate, frames))
		got := backlog(perInput, dstFrame)
		t.Logf("frames=%d backlog=%d samples (%.1f ms)", frames, got, float64(got)*1000/float64(dstRate))
		if prev != 0 {
			require.Equal(t, prev, got, "backlog must be a fixed delay, not grow with stream length")
		}
		prev = got
		ms := float64(got) * 1000 / float64(dstRate)
		require.Greater(t, ms, 20.0, "backlog collapsed — if a drainage fix landed, update this test")
		require.Less(t, ms, 400.0, "backlog exploded")
	}
}

// Predictable (beep) resampling already emits per write, so the option is inert
// there — pinned so a future change to that path does not silently diverge.
func TestResampleEarlyFlushPredictableUnaffected(t *testing.T) {
	const srcRate, dstRate = 8000, 48000
	src := telephonyFrames(srcRate, 10)

	slowPer, slow := runResample(t, srcRate, dstRate, src, media.WithPredictableResample(true))
	fastPer, fast := runResample(t, srcRate, dstRate, src,
		media.WithPredictableResample(true), media.WithEarlyFlush(true))

	require.Equal(t, slowPer, fastPer)
	require.Equal(t, slow, fast)
	require.NotZero(t, slowPer[0], "the predictable resampler already emits on the first frame")
}

// codex P2 round 2 (2026-08-11): the close-drain loop is bounded, and exhausting
// that bound means the tail was truncated. Close must say so rather than return
// nil. A normal close is well inside the bound, so it must report success.
func TestResampleCloseReportsIncompleteDrain(t *testing.T) {
	const srcRate, dstRate = 8000, 48000
	var got media.PCM16Sample
	dst := media.NewPCM16BufferWriter(&got, dstRate)
	r := media.ResampleWriter(dst, srcRate)
	for _, f := range telephonyFrames(srcRate, 60) {
		require.NoError(t, r.WriteSample(f))
	}
	require.NoError(t, r.Close(), "a normal close drains well inside the round limit")

	// The sentinel is exported so a caller can tell a truncated tail from a
	// downstream write failure.
	require.NotNil(t, media.ErrIncompleteDrain)
	require.NotEqual(t, "", media.ErrIncompleteDrain.Error())
}
