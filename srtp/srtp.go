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

package srtp

import (
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"sync"

	prtp "github.com/pion/rtp"
	"github.com/pion/srtp/v3"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/media-sdk/rtp"
)

var defaultProfiles = []ProtectionProfile{
	"AES_CM_128_HMAC_SHA1_80",
	"AES_CM_128_HMAC_SHA1_32",
	"AES_256_CM_HMAC_SHA1_80",
	"AES_256_CM_HMAC_SHA1_32",
}

func DefaultProfiles() ([]Profile, error) {
	out := make([]Profile, 0, len(defaultProfiles))
	for i, p := range defaultProfiles {
		sp, err := p.Parse()
		if err != nil {
			return nil, err
		}
		keyLen, err := sp.KeyLen()
		if err != nil {
			return nil, err
		}
		saltLen, err := sp.SaltLen()
		if err != nil {
			return nil, err
		}
		key := make([]byte, keyLen)
		salt := make([]byte, saltLen)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
		out = append(out, Profile{
			Index:   i + 1,
			Profile: p,
			Key:     key,
			Salt:    salt,
		})
	}
	return out, nil
}

type Options struct {
	Profiles []Profile
}

type ProtectionProfile string

func (p ProtectionProfile) Parse() (srtp.ProtectionProfile, error) {
	switch p {
	case "AES_CM_128_HMAC_SHA1_80":
		return srtp.ProtectionProfileAes128CmHmacSha1_80, nil
	case "AES_CM_128_HMAC_SHA1_32":
		return srtp.ProtectionProfileAes128CmHmacSha1_32, nil
	case "AES_256_CM_HMAC_SHA1_80":
		return srtp.ProtectionProfileAes256CmHmacSha1_80, nil
	case "AES_256_CM_HMAC_SHA1_32":
		return srtp.ProtectionProfileAes256CmHmacSha1_32, nil
	default:
		return 0, fmt.Errorf("unsupported profile %q", p)
	}
}

type Profile struct {
	Index    int
	Profile  ProtectionProfile
	Key      []byte
	Salt     []byte
	MKI      []byte // Master Key Identifier, nil if not present
	Lifetime uint64 // TODO: This is just a placeholder for future use
}

type Config = srtp.Config
type ContextOption = srtp.ContextOption
type SessionKeys = srtp.SessionKeys

// Expects a byte slice containing MKI value encoded in big-endian.
// Will be appended to packets we send.
func MasterKeyIndicator(mki []byte) ContextOption {
	return srtp.MasterKeyIndicator(mki)
}

func NewSession(log logger.Logger, conn net.Conn, conf *Config) (rtp.Session, error) {
	s, err := srtp.NewSessionSRTP(conn, conf)
	if err != nil {
		return nil, err
	}
	return &session{log: log, s: s}, nil
}

type session struct {
	log logger.Logger
	s   *srtp.SessionSRTP

	mu      sync.Mutex
	closed  bool
	streams []*srtp.ReadStreamSRTP
}

func (s *session) OpenWriteStream() (rtp.WriteStream, error) {
	w, err := s.s.OpenWriteStream()
	if err != nil {
		return nil, err
	}
	return writeStream{w: w}, nil
}

// afterAcceptHook, if non-nil, is called in AcceptStream after a stream has been
// accepted from the underlying pion session but before it is registered under the
// mutex, receiving the just-accepted stream. It exists only for tests, to
// deterministically drive a Close() into the accept-vs-close window and to capture
// the raw stream for close verification; it is nil (and therefore free) in normal
// operation.
var afterAcceptHook func(*srtp.ReadStreamSRTP)

func (s *session) AcceptStream() (rtp.ReadStream, uint32, error) {
	r, ssrc, err := s.s.AcceptStream()
	if err != nil {
		return nil, 0, err
	}
	if afterAcceptHook != nil {
		afterAcceptHook(r)
	}
	// Register the accepted stream under the mutex, atomically with checking whether
	// the session was already closed. pion/srtp does not close individual streams, so
	// we track them to close on our own Close(). Previously the stream was accepted
	// before this lock, so a Close() racing between AcceptStream returning and this
	// append snapshotted a streams slice that did not yet contain r -- leaking it
	// permanently: its pion buffer was never closed and any ReadRTP on it blocked
	// forever, hanging callers that join their readers.
	//
	// Guarantees now:
	//  1. No stream is left PERMANENTLY unclosed: a stream is either registered here
	//     (and closed by Close's drain) or, if the session is already closed, closed
	//     right below.
	//  2. No reader goroutine is ever started for a stream accepted during/after
	//     Close: AcceptStream returns io.EOF for it, which stops the caller's accept
	//     loop -- so callers that join their reader goroutines never hang.
	// Close() may return a moment before such a late stream's teardown completes, but
	// that stream is never handed out live and never read.
	s.mu.Lock()
	if s.closed {
		// The session was closed while (or before) this stream was accepted. Close()
		// has already drained the streams it knew about and will not see this one, so
		// tear it down here instead of returning a live-but-never-closed stream.
		s.mu.Unlock()
		_ = r.Close()
		return nil, 0, io.EOF
	}
	s.streams = append(s.streams, r)
	s.mu.Unlock()
	return readStream{r: r}, ssrc, nil
}

func (s *session) Close() error {
	err := s.s.Close() // Stop packets first

	s.mu.Lock()
	// Mark closed under the same lock AcceptStream registers under. Every stream is
	// then handled by exactly one path: one already registered is in the snapshot
	// below and closed here; one still mid-accept sees closed==true and is closed by
	// AcceptStream (which returns io.EOF, stopping the caller's accept loop so no
	// reader is spawned for it). Close may return just before that late teardown
	// finishes, but no stream is ever left permanently unclosed or handed out live.
	s.closed = true
	streams := s.streams
	s.streams = nil
	s.mu.Unlock()
	for _, r := range streams {
		_ = r.Close()
	}
	return err
}

type writeStream struct {
	w *srtp.WriteStreamSRTP
}

func (w writeStream) String() string {
	return "SRTPWriteStream"
}

func (w writeStream) WriteRTP(h *prtp.Header, payload []byte) (int, error) {
	return w.w.WriteRTP(h, payload)
}

type readStream struct {
	r *srtp.ReadStreamSRTP
}

func (r readStream) ReadRTP(h *prtp.Header, payload []byte) (int, error) {
	buf := payload
	n, err := r.r.Read(buf)
	if err != nil {
		return 0, err
	}
	var p rtp.Packet
	if err = p.Unmarshal(buf[:n]); err != nil {
		return 0, err
	}
	*h = p.Header
	n = copy(payload, p.Payload)
	return n, nil
}
