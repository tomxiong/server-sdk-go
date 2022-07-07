package lksdk

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/tomxiong/protocol/livekit"
	"github.com/tomxiong/protocol/utils"
	"go.uber.org/atomic"
)

const (
	rtpOutboundMTU = 1200
	rtpInboundMTU  = 1500
)

type SampleWriteOptions struct {
	AudioLevel *uint8
}

// LocalSampleTrack is a local track that simplifies writing samples.
// It handles timing and publishing of things, so as long as a SampleProvider is provided, the class takes care of
// publishing tracks at the right frequency
// This extends webrtc.TrackLocalStaticSample, and adds the ability to write RTP extensions
type LocalSampleTrack struct {
	packetizer      rtp.Packetizer
	sequencer       rtp.Sequencer
	transceiver     *webrtc.RTPTransceiver
	rtpTrack        *webrtc.TrackLocalStaticRTP
	ssrc            webrtc.SSRC
	ssrcAcked       bool
	clockRate       float64
	bound           atomic.Bool
	lock            sync.RWMutex
	audioLevelID    uint8
	sdesMidID       uint8
	sdesRtpStreamID uint8
	lastTS          time.Time
	simulcastID     string
	videoLayer      *livekit.VideoLayer
	onRTCP          func(rtcp.Packet)

	cancelWrite func()
	provider    SampleProvider
	onBind      func()
	onUnbind    func()
	// notify when sample provider responds with EOF
	onWriteComplete func()
}

type LocalSampleTrackOptions func(s *LocalSampleTrack)

// WithSimulcast marks the current track for simulcasting.
// In order to use simulcast, simulcastID must be identical across all layers
func WithSimulcast(simulcastID string, layer *livekit.VideoLayer) LocalSampleTrackOptions {
	return func(s *LocalSampleTrack) {
		s.videoLayer = layer
		s.simulcastID = simulcastID
	}
}

func WithRTCPHandler(cb func(rtcp.Packet)) LocalSampleTrackOptions {
	return func(s *LocalSampleTrack) {
		s.onRTCP = cb
	}
}

func NewLocalSampleTrack(c webrtc.RTPCodecCapability, opts ...LocalSampleTrackOptions) (*LocalSampleTrack, error) {
	s := &LocalSampleTrack{}
	for _, o := range opts {
		o(s)
	}
	rid := ""
	if s.videoLayer != nil {
		switch s.videoLayer.Quality {
		case livekit.VideoQuality_HIGH:
			rid = "f"
		case livekit.VideoQuality_MEDIUM:
			rid = "h"
		case livekit.VideoQuality_LOW:
			rid = "q"
		}
	}
	trackID := utils.NewGuid("TR_")
	streamID := utils.NewGuid("ST_")
	if s.simulcastID != "" {
		trackID = s.simulcastID
		streamID = s.simulcastID
	}
	rtpTrack, err := webrtc.NewTrackLocalStaticRTP(c, trackID, streamID, webrtc.WithRTPStreamID(rid))
	if err != nil {
		return nil, err
	}
	s.rtpTrack = rtpTrack
	return s, nil
}

func (s *LocalSampleTrack) SetTransceiver(transceiver *webrtc.RTPTransceiver) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.transceiver = transceiver
}

// ID is the unique identifier for this Track. This should be unique for the
// stream, but doesn't have to globally unique. A common example would be 'audio' or 'video'
// and StreamID would be 'desktop' or 'webcam'
func (s *LocalSampleTrack) ID() string { return s.rtpTrack.ID() }

// RID is the RTP stream identifier.
func (s *LocalSampleTrack) RID() string {
	return s.rtpTrack.RID()
}

// StreamID is the group this track belongs too. This must be unique
func (s *LocalSampleTrack) StreamID() string { return s.rtpTrack.StreamID() }

// Kind controls if this TrackLocal is audio or video
func (s *LocalSampleTrack) Kind() webrtc.RTPCodecType { return s.rtpTrack.Kind() }

// Codec gets the Codec of the track
func (s *LocalSampleTrack) Codec() webrtc.RTPCodecCapability {
	return s.rtpTrack.Codec()
}

func (s *LocalSampleTrack) IsBound() bool {
	return s.bound.Load()
}

// Bind is an interface for TrackLocal, not for external consumption
func (s *LocalSampleTrack) Bind(t webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	codec, err := s.rtpTrack.Bind(t)
	if err != nil {
		return codec, err
	}

	payloader, err := payloaderForCodec(codec.RTPCodecCapability)
	if err != nil {
		return codec, err
	}

	s.lock.Lock()
	s.ssrc = t.SSRC()
	for _, ext := range t.HeaderExtensions() {
		if ext.URI == sdp.AudioLevelURI {
			s.audioLevelID = uint8(ext.ID)
		}

		if ext.URI == sdp.SDESMidURI {
			s.sdesMidID = uint8(ext.ID)
		}

		if ext.URI == sdp.SDESRTPStreamIDURI {
			s.sdesRtpStreamID = uint8(ext.ID)
		}
	}
	s.sequencer = rtp.NewRandomSequencer()
	s.packetizer = rtp.NewPacketizer(
		rtpOutboundMTU,
		0, // Value is handled when writing
		0, // Value is handled when writing
		payloader,
		s.sequencer,
		codec.ClockRate,
	)
	s.clockRate = float64(codec.RTPCodecCapability.ClockRate)
	onBind := s.onBind
	provider := s.provider
	onWriteComplete := s.onWriteComplete
	s.bound.Store(true)
	s.lock.Unlock()

	if provider != nil {
		err = provider.OnBind()
		go s.writeWorker(provider, onWriteComplete)
	}

	go s.rtcpWorker(t.RTCPReader())

	// notify callbacks last
	if onBind != nil {
		go onBind()
	}
	return codec, err
}

// Unbind is an interface for TrackLocal, not for external consumption
func (s *LocalSampleTrack) Unbind(t webrtc.TrackLocalContext) error {
	s.lock.Lock()
	provider := s.provider
	onUnbind := s.onUnbind
	s.bound.Store(false)
	cancel := s.cancelWrite
	s.lock.Unlock()

	var err error

	if provider != nil {
		err = provider.OnUnbind()
	}
	if cancel != nil {
		cancel()
	}
	if onUnbind != nil {
		go onUnbind()
	}
	unbindErr := s.rtpTrack.Unbind(t)
	if unbindErr != nil {
		return unbindErr
	}
	return err
}

func (s *LocalSampleTrack) StartWrite(provider SampleProvider, onComplete func()) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.provider == provider {
		return nil
	}

	// when bound and already writing, ignore
	if s.IsBound() {
		// unbind previous provider
		if s.provider != nil {
			if err := s.provider.OnUnbind(); err != nil {
				return err
			}
		}
		if err := provider.OnBind(); err != nil {
			return err
		}
		// start new writer
		go s.writeWorker(provider, onComplete)
	}
	s.provider = provider
	s.onWriteComplete = onComplete
	return nil
}

// OnBind sets a callback to be called when the track has been negotiated for publishing and bound to a peer connection
func (s *LocalSampleTrack) OnBind(f func()) {
	s.lock.Lock()
	s.onBind = f
	s.lock.Unlock()
}

// OnUnbind sets a callback to be called after the track is removed from a peer connection
func (s *LocalSampleTrack) OnUnbind(f func()) {
	s.lock.Lock()
	s.onUnbind = f
	s.lock.Unlock()
}

func (s *LocalSampleTrack) WriteSample(sample media.Sample, opts *SampleWriteOptions) error {
	s.lock.RLock()
	p := s.packetizer
	clockRate := s.clockRate
	transceiver := s.transceiver
	ssrcAcked := s.ssrcAcked
	s.lock.RUnlock()

	if p == nil {
		return nil
	}

	// skip packets by the number of previously dropped packets
	for i := uint16(0); i < sample.PrevDroppedPackets; i++ {
		s.sequencer.NextSequenceNumber()
	}

	// calculate / interpolate duration when supplied duration is invalid
	if sample.Duration.Nanoseconds() < 0 {
		sample.Duration = sample.Timestamp.Sub(s.lastTS)
		s.lastTS = sample.Timestamp
	}

	samples := uint32(sample.Duration.Seconds() * clockRate)
	if sample.PrevDroppedPackets > 0 {
		p.SkipSamples(samples * uint32(sample.PrevDroppedPackets))
	}
	packets := p.Packetize(sample.Data, samples)

	var writeErrs []error
	for _, p := range packets {
		if s.audioLevelID != 0 && opts != nil && opts.AudioLevel != nil {
			ext := rtp.AudioLevelExtension{
				Level: *opts.AudioLevel,
			}
			data, err := ext.Marshal()
			if err != nil {
				writeErrs = append(writeErrs, err)
				continue
			}
			if err := p.Header.SetExtension(s.audioLevelID, data); err != nil {
				writeErrs = append(writeErrs, err)
				continue
			}
		}

		if s.RID() != "" && transceiver != nil && transceiver.Mid() != "" && !ssrcAcked {
			if s.sdesMidID != 0 {
				midValue := transceiver.Mid()
				if err := p.Header.SetExtension(s.sdesMidID, []byte(midValue)); err != nil {
					writeErrs = append(writeErrs, err)
					continue
				}
			}

			if s.sdesRtpStreamID != 0 {
				ridValue := s.RID()
				if err := p.Header.SetExtension(s.sdesRtpStreamID, []byte(ridValue)); err != nil {
					writeErrs = append(writeErrs, err)
					continue
				}
			}
		}

		if err := s.rtpTrack.WriteRTP(p); err != nil {
			writeErrs = append(writeErrs, err)
		}
	}

	if len(writeErrs) > 0 {
		return writeErrs[0]
	}

	return nil
}

func (s *LocalSampleTrack) rtcpWorker(rtcpReader interceptor.RTCPReader) {
	// read incoming rtcp packets, interceptors require this
	b := make([]byte, rtpInboundMTU)
	rtcpCB := s.onRTCP

	for {
		var a interceptor.Attributes
		i, _, err := rtcpReader.Read(b, a)
		if err != nil {
			// pipe closed
			return
		}

		pkts, err := rtcp.Unmarshal(b[:i])
		if err != nil {
			return
		}
		for _, packet := range pkts {
			s.lock.Lock()
			if !s.ssrcAcked {
				switch p := packet.(type) {
				case *rtcp.ReceiverReport:
					for _, r := range p.Reports {
						if webrtc.SSRC(r.SSRC) == s.ssrc {
							s.ssrcAcked = true
							break
						}
					}
				}
			}
			s.lock.Unlock()
			if rtcpCB != nil {
				rtcpCB(packet)
			}
		}
	}
}

func (s *LocalSampleTrack) writeWorker(provider SampleProvider, onComplete func()) {
	if s.cancelWrite != nil {
		s.cancelWrite()
	}
	var ctx context.Context
	s.lock.Lock()
	ctx, s.cancelWrite = context.WithCancel(context.Background())
	s.lock.Unlock()
	if onComplete != nil {
		defer onComplete()
	}

	audioProvider, isAudioProvider := provider.(AudioSampleProvider)

	nextSampleTime := time.Now()
	ticker := time.NewTicker(10 * time.Millisecond)
	for {
		sample, err := provider.NextSample()
		if err == io.EOF {
			return
		}
		if err != nil {
			logger.Error(err, "could not get sample from provider")
			return
		}

		var opts *SampleWriteOptions
		if isAudioProvider {
			level := audioProvider.CurrentAudioLevel()
			opts = &SampleWriteOptions{
				AudioLevel: &level,
			}
		}

		if err := s.WriteSample(sample, opts); err != nil {
			logger.Error(err, "could not write sample")
			return
		}
		// account for clock drift
		nextSampleTime = nextSampleTime.Add(sample.Duration)
		sleepDuration := time.Until(nextSampleTime)
		if sleepDuration < 0 {
			continue
		}
		ticker.Reset(sleepDuration)

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			return
		}
	}
}

// duplicated from pion mediaengine.go
func payloaderForCodec(codec webrtc.RTPCodecCapability) (rtp.Payloader, error) {
	switch strings.ToLower(codec.MimeType) {
	case strings.ToLower(webrtc.MimeTypeH264):
		return &codecs.H264Payloader{}, nil
	case strings.ToLower(webrtc.MimeTypeOpus):
		return &codecs.OpusPayloader{}, nil
	case strings.ToLower(webrtc.MimeTypeVP8):
		return &codecs.VP8Payloader{
			EnablePictureID: true,
		}, nil
	case strings.ToLower(webrtc.MimeTypeVP9):
		return &codecs.VP9Payloader{}, nil
	case strings.ToLower(webrtc.MimeTypeG722):
		return &codecs.G722Payloader{}, nil
	case strings.ToLower(webrtc.MimeTypePCMU), strings.ToLower(webrtc.MimeTypePCMA):
		return &codecs.G711Payloader{}, nil
	default:
		return nil, webrtc.ErrNoPayloaderForCodec
	}
}
