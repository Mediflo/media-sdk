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

package media

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"
)

var (
	resampleID         atomic.Uint32
	resampleDumpToFile = os.Getenv("LK_DUMP_RESAMPLE") == "true"
)

var DefaultResampleOptions []ResampleOption

// ErrIncompleteDrain is returned by a resample writer's Close when the resampler
// still had output to give after the bounded drain loop gave up. The writer is
// closed either way; the error says the very end of the stream was truncated.
//
// Declared here rather than next to the writer so it exists in every build
// configuration — the soxr writer is cgo-only, and callers (and tests) must be
// able to reference the sentinel with CGO_ENABLED=0 too.
var ErrIncompleteDrain = errors.New("resampler did not drain within the round limit")

// Resample the source sample into the destination sample rate.
// It appends resulting samples to dst and returns the result.
func Resample(dst PCM16Sample, dstSampleRate int, src PCM16Sample, srcSampleRate int, opts ...ResampleOption) PCM16Sample {
	if dstSampleRate == srcSampleRate {
		return append(dst, src...)
	}
	if len(opts) == 0 {
		opts = DefaultResampleOptions
	}
	var opt resampleOptions
	for _, o := range opts {
		o(&opt)
	}
	if opt.Predictable {
		return resampleBufferBeep(dst, dstSampleRate, src, srcSampleRate, &opt)
	}
	return resampleBuffer(dst, dstSampleRate, src, srcSampleRate, &opt)
}

type ResampleOption func(opts *resampleOptions)

func WithPredictableResample(enable bool) ResampleOption {
	return func(opts *resampleOptions) {
		opts.Predictable = enable
	}
}

func WithResampleDump(inputName, outputName string) ResampleOption {
	return func(opts *resampleOptions) {
		opts.DumpInput = inputName
		opts.DumpOutput = outputName
	}
}

// WithEarlyFlush makes the resample writer emit whatever the resampler has
// produced on every write, instead of withholding output until a full destination
// frame is available.
//
// The default (off) costs ONE destination frame of steady-state latency: a
// resampler typically returns slightly less than a full frame for the first
// input frame, so nothing is emitted, and from then on the writer runs one frame
// behind. On 20 ms telephony frames that is 20 ms per resampler in the path.
//
// The price is frame discontinuity: downstream sees short, irregular writes
// instead of neat frames. That is fine — and the latency is worth paying off —
// when the consumer re-frames anyway (a mixer input ring, a jitter buffer). It is
// NOT fine when the consumer turns each write into a packet: an RTP encoder would
// start emitting short packets at an irregular ptime. Enable it per call site,
// on the re-framing side only.
//
// No effect on the predictable (beep) resampler, which already emits per write.
func WithEarlyFlush(enable bool) ResampleOption {
	return func(opts *resampleOptions) {
		opts.EarlyFlush = enable
	}
}

type resampleOptions struct {
	Predictable bool
	EarlyFlush  bool
	DumpInput   string
	DumpOutput  string
}

// ResampleWriter returns a new writer that expects samples of a given sample rate
// and resamples then for the destination writer.
func ResampleWriter(w PCM16Writer, sampleRate int, opts ...ResampleOption) (w2 PCM16Writer) {
	srcRate := sampleRate
	dstRate := w.SampleRate()
	if dstRate == srcRate {
		return w
	}
	if len(opts) == 0 {
		opts = DefaultResampleOptions
	}
	var opt resampleOptions
	for _, o := range opts {
		o(&opt)
	}

	if resampleDumpToFile {
		id := resampleID.Add(1)
		pref := fmt.Sprintf("sip_resample_%d", id)
		opt.DumpInput = pref + "_in"
		opt.DumpOutput = pref + "_out"
	}
	if file := opt.DumpOutput; file != "" {
		w = DumpWriterPCM16(file, w)
	}
	if file := opt.DumpInput; file != "" {
		defer func() {
			w2 = DumpWriterPCM16(file, w2)
		}()
	}
	if opt.Predictable {
		return newResampleWriterBeep(w, sampleRate, &opt)
	}
	return newResampleWriter(w, sampleRate, &opt)
}
