package lksdk

import (
	"github.com/pion/webrtc/v3"
	"github.com/tomxiong/protocol/livekit"
)

type Track interface {
	ID() string
}

type TrackKind string

const (
	TrackKindVideo TrackKind = "video"
	TrackKindAudio TrackKind = "audio"
)

func (k TrackKind) String() string {
	return string(k)
}

func (k TrackKind) RTPType() webrtc.RTPCodecType {
	return webrtc.NewRTPCodecType(k.String())
}

func (k TrackKind) ProtoType() livekit.TrackType {
	switch k {
	case TrackKindAudio:
		return livekit.TrackType_AUDIO
	case TrackKindVideo:
		return livekit.TrackType_VIDEO
	}
	return livekit.TrackType(0)
}

func KindFromRTPType(rt webrtc.RTPCodecType) TrackKind {
	return TrackKind(rt.String())
}
