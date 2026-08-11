//go:build cgo

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

package media

import (
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func drainTestFrames(rate, n int) []PCM16Sample {
	frameSz := rate / 50
	out := make([]PCM16Sample, 0, n)
	phase := 0.0
	step := 2 * math.Pi * 440 / float64(rate)
	for i := 0; i < n; i++ {
		f := make(PCM16Sample, frameSz)
		for j := range f {
			f[j] = int16(8000 * math.Sin(phase))
			phase += step
		}
		out = append(out, f)
	}
	return out
}

// codex P2 round 3 (2026-08-11): the previous regression test only checked a
// normal close and that the sentinel existed, so reverting Close to its silent
// fallthrough would have left it green. This drives the bound down until the real
// resampler cannot finish inside it, which is the only way to reach the
// exhaustion path without faking the resampler.
func TestResampleCloseDrainExhaustion(t *testing.T) {
	const srcRate, dstRate = 8000, 48000
	frames := drainTestFrames(srcRate, 60)

	run := func(t *testing.T, rounds int) (error, int) {
		t.Helper()
		prev := maxCloseDrainRounds
		maxCloseDrainRounds = rounds
		t.Cleanup(func() { maxCloseDrainRounds = prev })

		var got PCM16Sample
		w := newResampleWriter(NewPCM16BufferWriter(&got, dstRate), srcRate, &resampleOptions{})
		for _, f := range frames {
			require.NoError(t, w.WriteSample(f))
		}
		return w.Close(), len(got)
	}

	t.Run("bound exhausted reports ErrIncompleteDrain", func(t *testing.T) {
		// One round cannot drain a ~180ms backlog: Resample(nil) only gets the
		// buffer's spare capacity per round.
		err, n := run(t, 1)
		require.ErrorIs(t, err, ErrIncompleteDrain,
			"a truncated tail must not be reported as a clean close")
		require.NotZero(t, n, "the writer must still have delivered the live stream")
	})

	t.Run("default bound drains cleanly", func(t *testing.T) {
		err, n := run(t, 64)
		require.NoError(t, err)
		require.Equal(t, len(frames)*(dstRate/50), n,
			"every sample fed in must come out when the drain completes")
	})

	// The sentinel must be matchable, and must not be confused with a downstream
	// Close failure — which wins, being the more actionable of the two.
	t.Run("downstream close error takes precedence", func(t *testing.T) {
		prev := maxCloseDrainRounds
		maxCloseDrainRounds = 1
		t.Cleanup(func() { maxCloseDrainRounds = prev })

		downstream := errors.New("downstream boom")
		var got PCM16Sample
		w := newResampleWriter(&failingCloseWriter{
			PCM16Writer: NewPCM16BufferWriter(&got, dstRate),
			err:         downstream,
		}, srcRate, &resampleOptions{})
		for _, f := range frames {
			require.NoError(t, w.WriteSample(f))
		}
		require.ErrorIs(t, w.Close(), downstream)
	})
}

type failingCloseWriter struct {
	PCM16Writer
	err error
}

func (w *failingCloseWriter) Close() error { return w.err }
