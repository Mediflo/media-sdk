// Copyright 2024 LiveKit, Inc.
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
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/res"
	"github.com/livekit/media-sdk/res/testdata"
	"github.com/livekit/media-sdk/webm"
)

type paceWriter struct {
	w       media.PCM16Writer
	mu      sync.Mutex
	cur     int
	frameT  []int
	frameSz []int
	samples int
}

func (p *paceWriter) Set(i int) {
	p.mu.Lock()
	p.cur = i
	p.mu.Unlock()
}

func (p *paceWriter) String() string {
	return fmt.Sprintf("paceWriter(%d) -> %s", p.cur, p.w.String())
}

func (p *paceWriter) SampleRate() int {
	return p.w.SampleRate()
}

func (p *paceWriter) WriteSample(sample media.PCM16Sample) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frameT = append(p.frameT, p.cur)
	p.frameSz = append(p.frameSz, len(sample))
	p.samples += len(sample)
	return p.w.WriteSample(sample)
}

func (p *paceWriter) Close() error {
	return nil
}

func TestResample(t *testing.T) {
	const (
		srcFile     = "resample.src.s16le"
		srcFileWebm = "resample.src.mka"
		srcRate     = res.SampleRate
	)
	srcFrames := res.ReadOggAudioFile(testdata.TestAudioOgg, res.SampleRate, 1)
	gotSrc := writePCM16s(t, srcFile, srcFrames)
	writePCM16sWebm(t, srcFileWebm, srcRate, srcFrames)
	require.True(t, gotSrc == "c774af95" || gotSrc == "b04a6a1f")

	// How the constants below come about: the writer holds output back until it has a
	// whole destination frame (resampleWriter.flush(minSize)), and every Resample call
	// is handed exactly one destination frame of output space (dstN in
	// soxrResampler.Resample). So the resampler's startup delay can never be made up
	// mid-stream. It surfaces three times over: as a late first frame (Skip), as a
	// single missed beat where the startup lag settles into the steady-state lag
	// (Bumps), and finally as the burst of frames Close drains out at the end
	// (Buffer).
	//
	// Since 042f394 ("resample: use SOXR_HQ instead of SOXR_LQ") that delay is much
	// larger, because HQ's longer filter needs materially more input before a full
	// destination frame exists. Every constant therefore grew, most at 8k where a
	// destination frame is only 160 samples and the same delay costs proportionally
	// more frames. The properties asserted are unchanged — only the numbers that
	// describe the delay moved. For the record, the SOXR_LQ values were
	// {16000, Skip: 1, Buffer: 1} and {8000, Skip: 2, Buffer: 3, Bumps: [211]}.
	for _, c := range []struct {
		Rate int
		// Skip is the source-frame tick at which the FIRST destination frame reaches
		// the writer: the resampler's startup latency, measured in frames.
		Skip int
		// Buffer is the number of trailing frames that Close releases by draining the
		// resampler. They all land on the last tick, so the steady-state pacing loop
		// stops short of them and they are checked separately below.
		Buffer int
		// Bumps lists ticks at which the writer legitimately receives nothing, shifting
		// every later frame one tick further. Under HQ there is exactly one per rate:
		// the point where the startup lag settles into the steady-state lag.
		Bumps []int
	}{
		{
			// 48k->16k, 320-sample frames: first frame at tick 1, no frame at tick 2,
			// steady lag of 2 ticks from there on; Close releases 3 frames.
			Rate: 16000, Skip: 1,
			Buffer: 3,
			Bumps:  []int{2},
		},
		{
			// 48k->8k, 160-sample frames: first frame at tick 7, no frame at tick 14,
			// steady lag of 8 ticks from there on; Close releases 9 frames.
			Rate: 8000, Skip: 7,
			Buffer: 9,
			Bumps:  []int{14},
		},
	} {
		t.Run(strconv.Itoa(c.Rate), func(t *testing.T) {
			resPref := fmt.Sprintf("resample.out.%d", c.Rate)
			resFile := resPref + ".s16le"
			resFileWebm := resPref + ".mka"

			var dstFrames []media.PCM16Sample
			dst := media.NewPCM16FrameWriter(&dstFrames, c.Rate)
			pw := &paceWriter{w: dst}
			r := media.ResampleWriter(pw, srcRate)
			defer r.Close()
			totalSamples := 0
			for i, src := range srcFrames {
				pw.Set(i)
				err := r.WriteSample(src)
				require.NoError(t, err)
				totalSamples += len(src)
			}
			r.Close()
			gotDst := writePCM16s(t, resFile, dstFrames)
			// only change if you validated the quality!
			// require.Equal(t, "???", gotDst) // TODO: resampler is numerically unstable
			_ = gotDst

			writePCM16sWebm(t, resFileWebm, c.Rate, dstFrames)

			expSamples := totalSamples / (srcRate / c.Rate)
			require.Equal(t, expSamples, pw.samples)
			require.Equal(t, len(srcFrames), len(pw.frameT))
			// Tick at which the first frame was received.
			skipped := pw.frameT[0]
			require.Equal(t, c.Skip, skipped)
			corr := 0
			for i, num := range pw.frameT {
				if i >= len(pw.frameT)-c.Buffer {
					break
				}
				exp := skipped + i + corr
				for _, b := range c.Bumps {
					if b == exp {
						corr++
						exp++
						break
					}
				}
				if exp != num {
					corr = num - (skipped + i)
					t.Errorf("skipped frame: exp %d, got %d\n%v", exp, num, pw.frameT)
				}
			}

			// The pacing loop above stops Buffer frames short of the end. Pin down what
			// those frames are instead of merely ignoring them: they must be exactly the
			// tail that Close drains, i.e. all of them land on the very last tick, and
			// nothing before them does. Otherwise a Buffer grown to accommodate the
			// resampler's latency would silently hide mid-stream bunching.
			lastTick := len(srcFrames) - 1
			for i := len(pw.frameT) - c.Buffer; i < len(pw.frameT); i++ {
				require.Equal(t, lastTick, pw.frameT[i], "frame %d must be part of the Close drain", i)
			}
			require.Less(t, pw.frameT[len(pw.frameT)-c.Buffer-1], lastTick,
				"only the last %d frames may land on the final tick", c.Buffer)

			// Frame continuity: with early flush off (see WithEarlyFlush) the writer owes
			// downstream whole destination frames, with at most a short remainder at the
			// very end. This is what the Buffer/Skip latency is being paid for.
			dstFrame := len(srcFrames[0]) / (srcRate / c.Rate)
			for i, sz := range pw.frameSz {
				if i == len(pw.frameSz)-1 {
					require.Positive(t, sz)
					require.LessOrEqual(t, sz, dstFrame, "trailing remainder must not exceed a frame")
					break
				}
				require.Equal(t, dstFrame, sz, "frame %d is not a whole destination frame", i)
			}
		})
	}

}

func writePCM16s(t testing.TB, path string, buf []media.PCM16Sample) string {
	var out []byte
	for _, frame := range buf {
		for _, v := range frame {
			var b [2]byte
			binary.LittleEndian.PutUint16(b[:], uint16(v))
			out = append(out, b[:]...)
		}
	}
	err := os.WriteFile(path, out, 0644)
	require.NoError(t, err)
	h := sha256.Sum256(out)
	return hex.EncodeToString(h[:])[:8]
}

func writePCM16sWebm(t testing.TB, path string, rate int, buf []media.PCM16Sample) {
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	w := webm.NewPCM16Writer(f, rate, 1, media.DefFrameDur)
	defer w.Close()

	for _, frame := range buf {
		err = w.WriteSample(frame)
		require.NoError(t, err)
	}
}

func memstats(pid int) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		panic(err)
	}
	fields := strings.Fields(string(data))
	v, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		panic(err)
	}
	return v
}

func TestResampleLeak(t *testing.T) {
	// memstats reads /proc/<pid>/statm, which is Linux-only — on macOS (and anywhere
	// else without procfs) it panics rather than measuring anything. Skip instead of
	// failing so `go test ./...` is usable on dev machines; CI runs Linux, where the
	// check is real.
	if runtime.GOOS != "linux" {
		t.Skipf("resampler leak check needs /proc/<pid>/statm, unavailable on %s", runtime.GOOS)
	}
	pid := os.Getpid()

	const (
		srcFile     = "resample.src.s16le"
		srcFileWebm = "resample.src.mka"
		srcRate     = res.SampleRate
	)
	srcFrames := res.ReadOggAudioFile(testdata.TestAudioOgg, res.SampleRate, 1)
	gotSrc := writePCM16s(t, srcFile, srcFrames)
	writePCM16sWebm(t, srcFileWebm, srcRate, srcFrames)
	require.True(t, gotSrc == "c774af95" || gotSrc == "b04a6a1f")

	const (
		runs           = 300
		pagesThreshold = 100
	)
	startMem := memstats(pid)
	captureStats := func(run int) {
		if t.Failed() {
			return
		}
		runtime.GC()

		endMem := memstats(pid)
		diff := endMem - startMem
		if diff > pagesThreshold {
			t.Fatalf("resampler memory leak detected (%d Kb / %d runs)", diff, run)
		}
	}
	defer captureStats(runs)

	for run := range runs {
		func() {
			defer captureStats(run + 1)
			var dstFrames []media.PCM16Sample
			dst := media.NewPCM16FrameWriter(&dstFrames, 16000)
			r := media.ResampleWriter(dst, srcRate)
			// This test the cleanup function, so intentionally avoid Close.
			//defer r.Close()
			totalSamples := 0
			for _, src := range srcFrames {
				err := r.WriteSample(src)
				require.NoError(t, err)
				totalSamples += len(src)
			}
		}()
	}
}
