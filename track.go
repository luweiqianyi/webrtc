// +build !js

package webrtc

import (
	"io"
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3/internal/util"
	"github.com/pion/webrtc/v3/pkg/media"
)

const (
	rtpOutboundMTU          = 1200
	trackDefaultIDLength    = 16
	trackDefaultLabelLength = 16
)

// Track represents a single media track
type Track struct {
	mu sync.RWMutex

	id          string
	payloadType uint8
	kind        RTPCodecType
	label       string
	ssrc        uint32
	codec       *RTPCodec
	rid         string

	packetizer rtp.Packetizer

	receiver         *RTPReceiver
	activeSenders    []*RTPSender
	totalSenderCount int // count of all senders (accounts for senders that have not been started yet)
	peeked           []byte
}

// ID gets the ID of the track
func (t *Track) ID() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.id
}

// RID gets the RTP Stream ID of this Track
// With Simulcast you will have multiple tracks with the same ID, but different RID values.
// In many cases a Track will not have an RID, so it is important to assert it is non-zero
func (t *Track) RID() string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.rid
}

// PayloadType gets the PayloadType of the track
func (t *Track) PayloadType() uint8 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.payloadType
}

// Kind gets the Kind of the track
func (t *Track) Kind() RTPCodecType {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.kind
}

// Label gets the Label of the track
func (t *Track) Label() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.label
}

// SSRC gets the SSRC of the track
func (t *Track) SSRC() uint32 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.ssrc
}

// Msid gets the Msid of the track
func (t *Track) Msid() string {
	return t.Label() + " " + t.ID()
}

// Codec gets the Codec of the track
func (t *Track) Codec() *RTPCodec {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.codec
}

// Packetizer gets the Packetizer of the track
func (t *Track) Packetizer() rtp.Packetizer {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.packetizer
}

// Read reads data from the track. If this is a local track this will error
func (t *Track) Read(b []byte) (n int, err error) {
	t.mu.RLock()
	r := t.receiver

	if t.totalSenderCount != 0 || r == nil {
		t.mu.RUnlock()
		return 0, errTrackLocalTrackRead
	}
	peeked := t.peeked != nil
	t.mu.RUnlock()

	if peeked {
		t.mu.Lock()
		data := t.peeked
		t.peeked = nil
		t.mu.Unlock()
		// someone else may have stolen our packet when we
		// released the lock.  Deal with it.
		if data != nil {
			n = copy(b, data)
			return
		}
	}

	return r.readRTP(b, t)
}

// peek is like Read, but it doesn't discard the packet read
func (t *Track) peek(b []byte) (n int, err error) {
	n, err = t.Read(b)
	if err != nil {
		return
	}

	t.mu.Lock()
	// this might overwrite data if somebody peeked between the Read
	// and us getting the lock.  Oh well, we'll just drop a packet in
	// that case.
	data := make([]byte, n)
	n = copy(data, b[:n])
	t.peeked = data
	t.mu.Unlock()
	return
}

// ReadRTP is a convenience method that wraps Read and unmarshals for you
func (t *Track) ReadRTP() (*rtp.Packet, error) {
	b := make([]byte, receiveMTU)
	i, err := t.Read(b)
	if err != nil {
		return nil, err
	}

	r := &rtp.Packet{}
	if err := r.Unmarshal(b[:i]); err != nil {
		return nil, err
	}
	return r, nil
}

// Write writes data to the track. If this is a remote track this will error
func (t *Track) Write(b []byte) (n int, err error) {
	packet := &rtp.Packet{}
	err = packet.Unmarshal(b)
	if err != nil {
		return 0, err
	}

	err = t.WriteRTP(packet)
	if err != nil {
		return 0, err
	}

	return len(b), nil
}

// WriteSample packetizes and writes to the track
func (t *Track) WriteSample(s media.Sample) error {
	packets := t.packetizer.Packetize(s.Data, s.Samples)
	for _, p := range packets {
		err := t.WriteRTP(p)
		if err != nil {
			return err
		}
	}

	return nil
}

// WriteRTP writes RTP packets to the track
func (t *Track) WriteRTP(p *rtp.Packet) error {
	t.mu.RLock()
	if t.receiver != nil {
		t.mu.RUnlock()
		return errTrackLocalTrackWrite
	}
	senders := t.activeSenders
	totalSenderCount := t.totalSenderCount
	t.mu.RUnlock()

	if totalSenderCount == 0 {
		return io.ErrClosedPipe
	}

	writeErrs := []error{}
	for _, s := range senders {
		if _, err := s.SendRTP(&p.Header, p.Payload); err != nil {
			writeErrs = append(writeErrs, err)
		}
	}

	return util.FlattenErrs(writeErrs)
}

// NewTrack initializes a new *Track
func NewTrack(payloadType uint8, ssrc uint32, id, label string, codec *RTPCodec) (*Track, error) {
	if ssrc == 0 {
		return nil, errTrackSSRCNewTrackZero
	}

	packetizer := rtp.NewPacketizer(
		rtpOutboundMTU,
		payloadType,
		ssrc,
		codec.Payloader,
		rtp.NewRandomSequencer(),
		codec.ClockRate,
	)

	return &Track{
		id:          id,
		payloadType: payloadType,
		kind:        codec.Type,
		label:       label,
		ssrc:        ssrc,
		codec:       codec,
		packetizer:  packetizer,
	}, nil
}

// determinePayloadType blocks and reads a single packet to determine the PayloadType for this Track
// this is useful if we are dealing with a remote track and we can't announce it to the user until we know the payloadType
func (t *Track) determinePayloadType() error {
	b := make([]byte, receiveMTU)
	n, err := t.peek(b)
	if err != nil {
		return err
	}
	r := rtp.Packet{}
	if err := r.Unmarshal(b[:n]); err != nil {
		return err
	}

	t.mu.Lock()
	t.payloadType = r.PayloadType
	defer t.mu.Unlock()

	return nil
}
