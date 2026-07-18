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
	"net"
	"sync"
	"testing"
	"time"

	prtp "github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/logger"

	"github.com/livekit/media-sdk/rtp"
)

// mirroredConfigs returns two SRTP Configs whose local/remote keys are mirror
// images, so the two sessions can decrypt each other's packets.
func mirroredConfigs(t *testing.T) (*Config, *Config) {
	t.Helper()
	profs, err := DefaultProfiles()
	require.NoError(t, err)
	p := profs[0]
	sp, err := p.Profile.Parse()
	require.NoError(t, err)

	localKey, localSalt := p.Key, p.Salt
	remoteKey := make([]byte, len(p.Key))
	remoteSalt := make([]byte, len(p.Salt))
	copy(remoteKey, p.Key)
	copy(remoteSalt, p.Salt)
	remoteKey[0] ^= 0xFF // distinct material for the two directions

	a := &Config{
		Keys: srtp.SessionKeys{
			LocalMasterKey:   localKey,
			LocalMasterSalt:  localSalt,
			RemoteMasterKey:  remoteKey,
			RemoteMasterSalt: remoteSalt,
		},
		Profile: sp,
	}
	b := &Config{
		Keys: srtp.SessionKeys{
			LocalMasterKey:   remoteKey,
			LocalMasterSalt:  remoteSalt,
			RemoteMasterKey:  localKey,
			RemoteMasterSalt: localSalt,
		},
		Profile: sp,
	}
	return a, b
}

// connectedUDP adapts an unconnected *net.UDPConn into a net.Conn that always
// sends to a fixed peer and reads any datagram (loopback, single peer).
type connectedUDP struct {
	*net.UDPConn
	peer *net.UDPAddr
}

func (c *connectedUDP) Write(b []byte) (int, error) { return c.UDPConn.WriteToUDP(b, c.peer) }
func (c *connectedUDP) RemoteAddr() net.Addr        { return c.peer }

// udpPair returns two loopback UDP sockets each addressed at the other.
func udpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	a, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	b, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	ca := &connectedUDP{UDPConn: a, peer: b.LocalAddr().(*net.UDPAddr)}
	cb := &connectedUDP{UDPConn: b, peer: a.LocalAddr().(*net.UDPAddr)}
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return ca, cb
}

func writeRTP(t *testing.T, w rtp.WriteStream, ssrc uint32, seq uint16) {
	t.Helper()
	h := &prtp.Header{Version: 2, SSRC: ssrc, SequenceNumber: seq, Timestamp: uint32(seq) * 160}
	_, _ = w.WriteRTP(h, make([]byte, 160))
}

// streamIsClosed reports whether a read stream has been closed (its buffer torn
// down), by draining any buffered packets and then confirming a further read
// returns an ERROR rather than blocking. A leaked (never-closed) stream's buffer
// read blocks forever once drained, so this returns false for a leak.
//
// The drain matters: a leaked stream may still have buffered packets that read
// out immediately; only a read PAST the buffered data distinguishes "closed"
// (returns io.EOF/closed error) from "leaked" (blocks forever).
func streamIsClosed(r rtp.ReadStream, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		type readRes struct {
			err error
		}
		res := make(chan readRes, 1)
		go func() {
			var h prtp.Header
			_, err := r.ReadRTP(&h, make([]byte, 1500))
			res <- readRes{err: err}
		}()
		select {
		case rr := <-res:
			if rr.err != nil {
				// A read returned an error -> the stream is closed. Done.
				return true
			}
			// A buffered packet read successfully; keep draining until the buffer
			// empties and the underlying read either errors (closed) or blocks (leak).
			continue
		case <-deadline:
			// A read blocked past the deadline -> the buffer is open and unfed:
			// the stream was never closed (leaked).
			return false
		}
	}
}

// TestSessionAcceptCloseRace_Deterministic uses the afterAcceptHook seam to force
// the exact accept-vs-close interleaving: a Close() runs AFTER a stream has been
// accepted from the underlying pion session but BEFORE session.AcceptStream
// registers it. Under the unpatched code Close snapshots a streams slice that does
// not yet contain that stream, so it is never closed and its ReadRTP blocks
// forever. The fix (register under the mutex + tear the stream down if the session
// is already closed) guarantees no accepted stream is left live-and-unclosed.
//
// This is the mandatory, DETERMINISTIC proof the fix works: it fails (blocks)
// against the unpatched code every run, and passes with the fix.
func TestSessionAcceptCloseRace_Deterministic(t *testing.T) {
	t.Cleanup(func() { afterAcceptHook = nil })
	log := logger.GetLogger()

	aConn, bConn := udpPair(t)
	aConf, bConf := mirroredConfigs(t)
	aSess, err := NewSession(log, aConn, aConf)
	require.NoError(t, err)
	bSess, err := NewSession(log, bConn, bConf)
	require.NoError(t, err)
	t.Cleanup(func() { _ = aSess.Close() })

	w, err := aSess.OpenWriteStream()
	require.NoError(t, err)

	// The hook fires once, on the first accepted stream, and runs Close() right in
	// the accept window before AcceptStream can register the stream. bSess.Close is
	// invoked synchronously from the hook so the interleaving is guaranteed.
	var hookOnce sync.Once
	closeReturned := make(chan struct{})
	afterAcceptHook = func() {
		hookOnce.Do(func() {
			_ = bSess.Close()
			close(closeReturned)
		})
	}

	// Drive one stream into AcceptStream and capture what it returns.
	type acceptResult struct {
		r   rtp.ReadStream
		err error
	}
	resCh := make(chan acceptResult, 1)
	go func() {
		writeRTP(t, w, 0x1234, 1)
		r, _, aerr := bSess.AcceptStream()
		resCh <- acceptResult{r: r, err: aerr}
	}()

	select {
	case <-closeReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("hook/Close never fired; test could not create the race window")
	}

	var res acceptResult
	select {
	case res = <-resCh:
	case <-time.After(5 * time.Second):
		t.Fatal("AcceptStream never returned after Close")
	}

	// Whatever AcceptStream returned, it must not leave a live-but-unclosed stream:
	//   - fix path: it returns (nil, io.EOF) after closing the late stream; OR
	//   - it returns a live stream that Close DID manage to close.
	// In every case a ReadRTP on any returned live stream must NOT block forever.
	if res.err == nil {
		require.NotNil(t, res.r)
		require.True(t, streamIsClosed(res.r, 2*time.Second),
			"accepted stream's ReadRTP blocked after Close -- stream leaked (accept-vs-close race)")
	} else {
		require.Nil(t, res.r, "no live stream should be returned alongside an error")
	}
}

// TestSessionAcceptCloseRace_Stress races AcceptStream against Close with real
// traffic and no hooks, as a belt-and-suspenders check across many iterations
// under -race.
func TestSessionAcceptCloseRace_Stress(t *testing.T) {
	t.Cleanup(func() { afterAcceptHook = nil })
	log := logger.GetLogger()

	for iter := 0; iter < 100; iter++ {
		aConn, bConn := udpPair(t)
		aConf, bConf := mirroredConfigs(t)
		aSess, err := NewSession(log, aConn, aConf)
		require.NoError(t, err)
		bSess, err := NewSession(log, bConn, bConf)
		require.NoError(t, err)

		w, err := aSess.OpenWriteStream()
		require.NoError(t, err)
		senderStop := make(chan struct{})
		var senderWG sync.WaitGroup
		senderWG.Add(1)
		go func() {
			defer senderWG.Done()
			ssrc := uint32(iter+1) * 100000
			var seq uint16
			for {
				select {
				case <-senderStop:
					return
				default:
					ssrc++
					seq++
					writeRTP(t, w, ssrc, seq)
				}
			}
		}()

		var mu sync.Mutex
		var accepted []rtp.ReadStream
		var acceptWG sync.WaitGroup
		acceptWG.Add(1)
		go func() {
			defer acceptWG.Done()
			for {
				r, _, aerr := bSess.AcceptStream()
				if aerr != nil {
					return
				}
				mu.Lock()
				accepted = append(accepted, r)
				mu.Unlock()
			}
		}()

		time.Sleep(time.Duration(iter%5) * time.Millisecond)
		require.NoError(t, bSess.Close())
		acceptWG.Wait()
		close(senderStop)
		senderWG.Wait()
		_ = aSess.Close()

		mu.Lock()
		streams := accepted
		mu.Unlock()
		for i, r := range streams {
			require.True(t, streamIsClosed(r, 2*time.Second),
				"iter %d stream %d: ReadRTP blocked after Close -- stream leaked", iter, i)
		}
	}
}
