package rtc

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/rtcerr"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/livekit-server/pkg/telemetry"
)

const (
	initialQualityUpdateWait = 10 * time.Second
)

// MediaTrackSubscriptions manages subscriptions of a media track
type MediaTrackSubscriptions struct {
	params MediaTrackSubscriptionsParams

	subscribedTracks sync.Map // participantID => types.SubscribedTrack

	// quality level enable/disable
	maxQualityLock               sync.RWMutex
	maxSubscriberQuality         map[livekit.ParticipantID]livekit.VideoQuality
	maxSubscriberNodeQuality     map[string]livekit.VideoQuality // nodeID => livekit.VideoQuality
	maxSubscribedQuality         livekit.VideoQuality
	onSubscribedMaxQualityChange func(subscribedQualities []*livekit.SubscribedQuality, maxSubscribedQuality livekit.VideoQuality)
	maxQualityTimer              *time.Timer
}

type MediaTrackSubscriptionsParams struct {
	MediaTrack types.MediaTrack

	BufferFactory    *buffer.Factory
	ReceiverConfig   ReceiverConfig
	SubscriberConfig DirectionConfig

	Telemetry telemetry.TelemetryService

	Logger logger.Logger
}

func NewMediaTrackSubscriptions(params MediaTrackSubscriptionsParams) *MediaTrackSubscriptions {
	t := &MediaTrackSubscriptions{
		params:               params,
		maxSubscriberQuality: make(map[livekit.ParticipantID]livekit.VideoQuality),
	}

	return t
}

func (t *MediaTrackSubscriptions) SetMuted(muted bool) {
	// mute all subscribed tracks
	t.subscribedTracks.Range(func(_, value interface{}) bool {
		if st, ok := value.(types.SubscribedTrack); ok {
			st.SetPublisherMuted(muted)
		}
		return true
	})

	// update quality based on subscription if unmuting
	if !muted {
		t.updateQualityChange()
	}
}

func (t *MediaTrackSubscriptions) IsSubscriber(subID livekit.ParticipantID) bool {
	_, ok := t.subscribedTracks.Load(subID)
	return ok
}

// AddSubscriber subscribes sub to current mediaTrack
func (t *MediaTrackSubscriptions) AddSubscriber(sub types.Participant, codec webrtc.RTPCodecCapability, wr WrappedReceiver) (*sfu.DownTrack, error) {
	subscriberID := sub.ID()

	// don't subscribe to the same track multiple times
	if _, ok := t.subscribedTracks.Load(subscriberID); ok {
		return nil, nil
	}

	var rtcpFeedback []webrtc.RTCPFeedback
	switch t.params.MediaTrack.Kind() {
	case livekit.TrackType_AUDIO:
		rtcpFeedback = t.params.SubscriberConfig.RTCPFeedback.Audio
	case livekit.TrackType_VIDEO:
		rtcpFeedback = t.params.SubscriberConfig.RTCPFeedback.Video
	}
	downTrack, err := sfu.NewDownTrack(webrtc.RTPCodecCapability{
		MimeType:     codec.MimeType,
		ClockRate:    codec.ClockRate,
		Channels:     codec.Channels,
		SDPFmtpLine:  codec.SDPFmtpLine,
		RTCPFeedback: rtcpFeedback,
	}, wr, t.params.BufferFactory, subscriberID, t.params.ReceiverConfig.PacketBufferSize)
	if err != nil {
		return nil, err
	}
	subTrack := NewSubscribedTrack(SubscribedTrackParams{
		PublisherID:       t.params.MediaTrack.ParticipantID(),
		PublisherIdentity: t.params.MediaTrack.ParticipantIdentity(),
		SubscriberID:      subscriberID,
		MediaTrack:        t.params.MediaTrack,
		DownTrack:         downTrack,
	})

	var transceiver *webrtc.RTPTransceiver
	var sender *webrtc.RTPSender
	if sub.ProtocolVersion().SupportsTransceiverReuse() {
		//
		// AddTrack will create a new transceiver or re-use an unused one
		// if the attributes match. This prevents SDP from bloating
		// because of dormant transceivers building up.
		//
		sender, err = sub.SubscriberPC().AddTrack(downTrack)
		if err != nil {
			return nil, err
		}

		// as there is no way to get transceiver from sender, search
		for _, tr := range sub.SubscriberPC().GetTransceivers() {
			if tr.Sender() == sender {
				transceiver = tr
				break
			}
		}
		if transceiver == nil {
			// cannot add, no transceiver
			return nil, errors.New("cannot subscribe without a transceiver in place")
		}
	} else {
		transceiver, err = sub.SubscriberPC().AddTransceiverFromTrack(downTrack, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionSendonly,
		})
		if err != nil {
			return nil, err
		}

		sender = transceiver.Sender()
		if sender == nil {
			// cannot add, no sender
			return nil, errors.New("cannot subscribe without a sender in place")
		}
	}

	sendParameters := sender.GetParameters()
	downTrack.SetRTPHeaderExtensions(sendParameters.HeaderExtensions)

	downTrack.SetTransceiver(transceiver)
	// when outtrack is bound, start loop to send reports
	downTrack.OnBind(func() {
		go subTrack.Bound()
		go t.sendDownTrackBindingReports(sub)
	})
	downTrack.OnPacketSent(func(_ *sfu.DownTrack, size int) {
		if t.params.Telemetry != nil {
			t.params.Telemetry.OnDownstreamPacket(subscriberID, t.params.MediaTrack.ID(), size)
		}
	})
	downTrack.OnPaddingSent(func(_ *sfu.DownTrack, size int) {
		if t.params.Telemetry != nil {
			t.params.Telemetry.OnDownstreamPacket(subscriberID, t.params.MediaTrack.ID(), size)
		}
	})
	downTrack.OnRTCP(func(pkts []rtcp.Packet) {
		if t.params.Telemetry != nil {
			t.params.Telemetry.HandleRTCP(livekit.StreamType_DOWNSTREAM, subscriberID, t.params.MediaTrack.ID(), pkts)
		}
	})

	downTrack.OnCloseHandler(func() {
		go func() {
			t.subscribedTracks.Delete(subscriberID)
			if t.params.Telemetry != nil {
				t.params.Telemetry.TrackUnsubscribed(context.Background(), subscriberID, t.params.MediaTrack.ToProto())
			}

			// ignore if the subscribing sub is not connected
			if sub.SubscriberPC().ConnectionState() == webrtc.PeerConnectionStateClosed {
				return
			}

			// if the source has been terminated, we'll need to terminate all of the subscribedtracks
			// however, if the dest sub has disconnected, then we can skip
			if sender == nil {
				return
			}
			t.params.Logger.Debugw("removing peerconnection track",
				"track", t.params.MediaTrack.ID(),
				"subscriber", sub.Identity(),
				"subscriberID", subscriberID,
				"kind", t.params.MediaTrack.Kind(),
			)
			if err := sub.SubscriberPC().RemoveTrack(sender); err != nil {
				if err == webrtc.ErrConnectionClosed {
					// sub closing, can skip removing subscribedtracks
					return
				}
				if _, ok := err.(*rtcerr.InvalidStateError); !ok {
					// most of these are safe to ignore, since the track state might have already
					// been set to Inactive
					t.params.Logger.Debugw("could not remove remoteTrack from forwarder",
						"error", err,
						"subscriber", sub.Identity(),
						"subscriberID", subscriberID,
					)
				}
			}

			t.NotifySubscriberMaxQuality(subscriberID, livekit.VideoQuality_OFF)
			sub.RemoveSubscribedTrack(subTrack)
			sub.Negotiate()
		}()
	})

	t.subscribedTracks.Store(subscriberID, subTrack)
	subTrack.SetPublisherMuted(t.params.MediaTrack.IsMuted())

	// since sub will lock, run it in a goroutine to avoid deadlocks
	go func() {
		t.NotifySubscriberMaxQuality(subscriberID, livekit.VideoQuality_HIGH) // start with HIGH, let subscription change it later
		sub.AddSubscribedTrack(subTrack)
		sub.Negotiate()
	}()

	if t.params.Telemetry != nil {
		t.params.Telemetry.TrackSubscribed(context.Background(), subscriberID, t.params.MediaTrack.ToProto())
	}
	return downTrack, nil
}

// RemoveSubscriber removes participant from subscription
// stop all forwarders to the client
func (t *MediaTrackSubscriptions) RemoveSubscriber(participantID livekit.ParticipantID) {
	subTrack := t.getSubscribedTrack(participantID)
	if subTrack != nil {
		go subTrack.DownTrack().Close()
	}
}

func (t *MediaTrackSubscriptions) RemoveAllSubscribers() {
	t.params.Logger.Debugw("removing all subscribers", "track", t.params.MediaTrack.ID())

	t.subscribedTracks.Range(func(_, val interface{}) bool {
		if subTrack, ok := val.(types.SubscribedTrack); ok {
			go subTrack.DownTrack().Close()
		}
		return true
	})
	t.subscribedTracks = sync.Map{}
}

func (t *MediaTrackSubscriptions) RevokeDisallowedSubscribers(allowedSubscriberIDs []livekit.ParticipantID) []livekit.ParticipantID {
	var revokedSubscriberIDs []livekit.ParticipantID
	// LK-TODO: large number of subscribers needs to be solved for this loop
	t.subscribedTracks.Range(func(key interface{}, val interface{}) bool {
		if subID, ok := key.(livekit.ParticipantID); ok {
			found := false
			for _, allowedID := range allowedSubscriberIDs {
				if subID == allowedID {
					found = true
					break
				}
			}

			if !found {
				if subTrack, ok := val.(types.SubscribedTrack); ok {
					go subTrack.DownTrack().Close()
					revokedSubscriberIDs = append(revokedSubscriberIDs, subID)
				}
			}
		}
		return true
	})

	return revokedSubscriberIDs
}

func (t *MediaTrackSubscriptions) UpdateVideoLayers() {
	t.subscribedTracks.Range(func(_, val interface{}) bool {
		if st, ok := val.(types.SubscribedTrack); ok {
			st.UpdateVideoLayer()
		}
		return true
	})
}

func (t *MediaTrackSubscriptions) getSubscribedTrack(subscriberID livekit.ParticipantID) types.SubscribedTrack {
	if val, ok := t.subscribedTracks.Load(subscriberID); ok {
		if st, ok := val.(types.SubscribedTrack); ok {
			return st
		}
	}
	return nil
}

// TODO: send for all downtracks from the source participant
// https://tools.ietf.org/html/rfc7941
func (t *MediaTrackSubscriptions) sendDownTrackBindingReports(sub types.Participant) {
	var sd []rtcp.SourceDescriptionChunk

	subTrack := t.getSubscribedTrack(sub.ID())
	if subTrack == nil {
		return
	}

	chunks := subTrack.DownTrack().CreateSourceDescriptionChunks()
	if chunks == nil {
		return
	}
	sd = append(sd, chunks...)

	pkts := []rtcp.Packet{
		&rtcp.SourceDescription{Chunks: sd},
	}

	go func() {
		defer RecoverSilent()
		batch := pkts
		i := 0
		for {
			if err := sub.SubscriberPC().WriteRTCP(batch); err != nil {
				t.params.Logger.Errorw("could not write RTCP", err)
				return
			}
			if i > 5 {
				return
			}
			i++
			time.Sleep(20 * time.Millisecond)
		}
	}()
}

func (t *MediaTrackSubscriptions) DebugInfo() []map[string]interface{} {
	subscribedTrackInfo := make([]map[string]interface{}, 0)
	t.subscribedTracks.Range(func(_, val interface{}) bool {
		if track, ok := val.(*SubscribedTrack); ok {
			dt := track.DownTrack().DebugInfo()
			dt["PubMuted"] = track.pubMuted.Get()
			dt["SubMuted"] = track.subMuted.Get()
			subscribedTrackInfo = append(subscribedTrackInfo, dt)
		}
		return true
	})

	return subscribedTrackInfo
}

func (t *MediaTrackSubscriptions) OnSubscribedMaxQualityChange(f func(subscribedQualities []*livekit.SubscribedQuality, maxSubscribedQuality livekit.VideoQuality)) {
	t.onSubscribedMaxQualityChange = f
}

func (t *MediaTrackSubscriptions) NotifySubscriberMaxQuality(subscriberID livekit.ParticipantID, quality livekit.VideoQuality) {
	if t.params.MediaTrack.Kind() != livekit.TrackType_VIDEO {
		return
	}

	t.maxQualityLock.Lock()
	if quality == livekit.VideoQuality_OFF {
		_, ok := t.maxSubscriberQuality[subscriberID]
		if !ok {
			t.maxQualityLock.Unlock()
			return
		}

		delete(t.maxSubscriberQuality, subscriberID)
	} else {
		maxQuality, ok := t.maxSubscriberQuality[subscriberID]
		if ok && maxQuality == quality {
			t.maxQualityLock.Unlock()
			return
		}

		t.maxSubscriberQuality[subscriberID] = quality
	}
	t.maxQualityLock.Unlock()

	t.updateQualityChange()
}

func (t *MediaTrackSubscriptions) NotifySubscriberNodeMaxQuality(nodeID string, quality livekit.VideoQuality) {
	if t.params.MediaTrack.Kind() != livekit.TrackType_VIDEO {
		return
	}

	t.maxQualityLock.Lock()
	if quality == livekit.VideoQuality_OFF {
		_, ok := t.maxSubscriberNodeQuality[nodeID]
		if !ok {
			t.maxQualityLock.Unlock()
			return
		}

		delete(t.maxSubscriberNodeQuality, nodeID)
	} else {
		maxQuality, ok := t.maxSubscriberNodeQuality[nodeID]
		if ok && maxQuality == quality {
			t.maxQualityLock.Unlock()
			return
		}

		t.maxSubscriberNodeQuality[nodeID] = quality
	}
	t.maxQualityLock.Unlock()

	t.updateQualityChange()
}

func (t *MediaTrackSubscriptions) startMaxQualityTimer() {
	t.maxQualityLock.Lock()
	defer t.maxQualityLock.Unlock()

	if t.params.MediaTrack.Kind() != livekit.TrackType_VIDEO {
		return
	}

	t.maxQualityTimer = time.AfterFunc(initialQualityUpdateWait, func() {
		t.stopMaxQualityTimer()
		t.updateQualityChange()
	})
}

func (t *MediaTrackSubscriptions) stopMaxQualityTimer() {
	t.maxQualityLock.Lock()
	defer t.maxQualityLock.Unlock()

	if t.maxQualityTimer != nil {
		t.maxQualityTimer.Stop()
		t.maxQualityTimer = nil
	}
}

func (t *MediaTrackSubscriptions) updateQualityChange() {
	if t.params.MediaTrack.Kind() != livekit.TrackType_VIDEO {
		return
	}

	t.maxQualityLock.Lock()
	maxSubscribedQuality := livekit.VideoQuality_OFF
	for _, subQuality := range t.maxSubscriberQuality {
		if maxSubscribedQuality == livekit.VideoQuality_OFF || subQuality > maxSubscribedQuality {
			maxSubscribedQuality = subQuality
		}
	}

	for _, subQuality := range t.maxSubscriberNodeQuality {
		if maxSubscribedQuality == livekit.VideoQuality_OFF || subQuality > maxSubscribedQuality {
			maxSubscribedQuality = subQuality
		}
	}

	if maxSubscribedQuality == t.maxSubscribedQuality {
		t.maxQualityLock.Unlock()
		return
	}

	t.maxSubscribedQuality = maxSubscribedQuality

	var subscribedQualities []*livekit.SubscribedQuality
	if t.maxSubscribedQuality == livekit.VideoQuality_OFF {
		subscribedQualities = []*livekit.SubscribedQuality{
			{Quality: livekit.VideoQuality_LOW, Enabled: false},
			{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
			{Quality: livekit.VideoQuality_HIGH, Enabled: false},
		}
	} else {
		for q := livekit.VideoQuality_LOW; q <= livekit.VideoQuality_HIGH; q++ {
			subscribedQualities = append(subscribedQualities, &livekit.SubscribedQuality{
				Quality: q,
				Enabled: q <= t.maxSubscribedQuality,
			})
		}
	}
	t.maxQualityLock.Unlock()

	if t.onSubscribedMaxQualityChange != nil {
		t.onSubscribedMaxQualityChange(subscribedQualities, maxSubscribedQuality)
	}
}

func (t *MediaTrackSubscriptions) numSubscribed() uint32 {
	t.maxQualityLock.RLock()
	numSubscribed := uint32(0)
	if t.maxSubscribedQuality != livekit.VideoQuality_OFF {
		numSubscribed = uint32(SpatialLayerForQuality(t.maxSubscribedQuality) + 1)
	}
	t.maxQualityLock.RUnlock()

	return numSubscribed
}