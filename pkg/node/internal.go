package sfu

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lucsky/cuid"
	sdptransform "github.com/notedit/sdp"
	"github.com/pion/ion-sfu/pkg/log"
	"github.com/pion/ion-sfu/pkg/rtc"
	transport "github.com/pion/ion-sfu/pkg/rtc/transport"
	"github.com/pion/webrtc/v2"

	pb "github.com/pion/ion-sfu/pkg/proto"
)

func handleTrickle(r *rtc.Router, t *transport.WebRTCTransport) {
	// for {
	// 	trickle := <-t.GetCandidateChan()
	// 	if trickle != nil {
	// 		broadcaster.Say(proto.SFUTrickleICE, util.Map("mid", t.ID(), "trickle", trickle.ToJSON()))
	// 	} else {
	// 		return
	// 	}
	// }
}

// Publish a stream to the sfu
func (s *server) Publish(in *pb.PublishRequest, out pb.SFU_PublishServer) error {
	log.Infof("publish msg=%v", in)
	if in.Description.Sdp == "" {
		return errors.New("publish: jsep invaild")
	}
	mid := cuid.New()
	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: in.Description.Sdp}

	rtcOptions := transport.RTCOptions{
		Publish:     true,
		Codec:       in.Options.Codec,
		Bandwidth:   in.Options.Bandwidth,
		TransportCC: in.Options.Transportcc,
	}

	videoCodec := strings.ToUpper(rtcOptions.Codec)

	sdpObj, err := sdptransform.Parse(offer.SDP)
	if err != nil {
		log.Errorf("err=%v sdpObj=%v", err, sdpObj)
		return errors.New("publish: sdp parse failed")
	}

	allowedCodecs := make([]uint8, 0)
	for _, s := range sdpObj.GetStreams() {
		for _, track := range s.GetTracks() {
			pt, _ := getPubPTForTrack(videoCodec, track, sdpObj)

			if len(track.GetSSRCS()) == 0 {
				return errors.New("publish: ssrc not found")
			}
			allowedCodecs = append(allowedCodecs, pt)
		}
	}

	rtcOptions.Codecs = allowedCodecs
	pub := transport.NewWebRTCTransport(mid, rtcOptions)
	if pub == nil {
		return errors.New("publish: transport.NewWebRTCTransport failed")
	}

	router := rtc.GetOrNewRouter(mid)

	go handleTrickle(router, pub)

	answer, err := pub.Answer(offer, rtcOptions)
	if err != nil {
		log.Errorf("err=%v answer=%v", err, answer)
		return errors.New("publish: pub.Answer failed")
	}

	router.AddPub(pub)

	log.Infof("publish answer = %v", answer)

	err = out.Send(&pb.PublishReply{
		Mid: mid,
		Description: &pb.SessionDescription{
			Type: answer.Type.String(),
			Sdp:  answer.SDP,
		},
	})

	if err != nil {
		log.Errorf("Error publishing stream")
		return err
	}

	<-router.CloseChan
	return nil
}

// Unpublish a stream
func (s *server) Unpublish(ctx context.Context, in *pb.UnpublishRequest) (*pb.UnpublishReply, error) {
	log.Infof("unpublish msg=%v", in)

	mid := in.Mid
	router := rtc.GetOrNewRouter(mid)
	if router != nil {
		rtc.DelRouter(mid)
		return &pb.UnpublishReply{}, nil
	}
	return nil, errors.New("unpublish: Router not found")
}

// Subscribe to a stream
func (s *server) Subscribe(ctx context.Context, in *pb.SubscribeRequest) (*pb.SubscribeReply, error) {
	log.Infof("subscribe msg=%v", in)
	router := rtc.GetOrNewRouter(in.Mid)

	if router == nil {
		return nil, errors.New("subscribe: router not found")
	}

	pub := router.GetPub().(*transport.WebRTCTransport)

	if in.Description.Sdp == "" {
		return nil, errors.New("subscribe: unsupported media type")
	}

	sdp := in.Description.Sdp

	rtcOptions := transport.RTCOptions{
		Subscribe: true,
	}

	if in.Options != nil {
		if in.Options.Bandwidth != 0 {
			rtcOptions.Bandwidth = in.Options.Bandwidth
		}

		rtcOptions.TransportCC = in.Options.Transportcc
	}

	subID := cuid.New()

	tracks := pub.GetInTracks()
	rtcOptions.Ssrcpt = make(map[uint32]uint8)

	for ssrc, track := range tracks {
		rtcOptions.Ssrcpt[ssrc] = uint8(track.PayloadType())
	}

	sdpObj, err := sdptransform.Parse(sdp)
	if err != nil {
		log.Errorf("err=%v sdpObj=%v", err, sdpObj)
		return nil, errors.New("subscribe: sdp parse failed")
	}

	ssrcPTMap := make(map[uint32]uint8)
	allowedCodecs := make([]uint8, 0, len(tracks))

	for ssrc, track := range tracks {
		// Find pt for track given track.Payload and sdp
		ssrcPTMap[ssrc] = getSubPTForTrack(track, sdpObj)
		allowedCodecs = append(allowedCodecs, ssrcPTMap[ssrc])
	}

	// Set media engine codecs based on found pts
	log.Infof("Allowed codecs %v", allowedCodecs)
	rtcOptions.Codecs = allowedCodecs

	// New api
	sub := transport.NewWebRTCTransport(subID, rtcOptions)

	if sub == nil {
		return nil, errors.New("subscribe: transport.NewWebRTCTransport failed")
	}

	go handleTrickle(router, sub)

	for ssrc, track := range tracks {
		// Get payload type from request track
		pt := track.PayloadType()
		if newPt, ok := ssrcPTMap[ssrc]; ok {
			// Override with "negotiated" PT
			pt = newPt
		}

		// I2AacsRLsZZriGapnvPKiKBcLi8rTrO1jOpq c84ded42-d2b0-4351-88d2-b7d240c33435
		//                streamID                        trackID
		log.Infof("AddTrack: codec:%s, ssrc:%d, pt:%d, streamID %s, trackID %s", track.Codec().MimeType, ssrc, pt, pub.ID(), track.ID())
		_, err := sub.AddSendTrack(ssrc, pt, pub.ID(), track.ID())
		if err != nil {
			log.Errorf("err=%v", err)
		}
	}

	// Build answer
	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}
	answer, err := sub.Answer(offer, rtcOptions)
	if err != nil {
		log.Errorf("err=%v answer=%v", err, answer)
		return nil, errors.New("unsupported media type")
	}

	router.AddSub(subID, sub)

	log.Infof("subscribe mid %s, answer = %v", subID, answer)
	return &pb.SubscribeReply{
		Mid: subID,
		Description: &pb.SessionDescription{
			Type: answer.Type.String(),
			Sdp:  answer.SDP,
		},
	}, nil
}

// Unsubscribe from a stream
func (s *server) Unsubscribe(ctx context.Context, in *pb.UnsubscribeRequest) (*pb.UnsubscribeReply, error) {
	log.Infof("unsubscribe msg=%v", in)
	mid := in.Mid
	found := false
	rtc.MapRouter(func(id string, r *rtc.Router) {
		subs := r.GetSubs()
		for sid := range subs {
			if sid == mid {
				r.DelSub(mid)
				found = true
				return
			}
		}
	})
	if found {
		return &pb.UnsubscribeReply{}, nil
	}
	return nil, fmt.Errorf("unsubscribe: sub [%s] not found", mid)
}

// func trickle(msg map[string]interface{}) (map[string]interface{}, *nprotoo.Error) {
// 	log.Infof("trickle msg=%v", msg)
// 	router := util.Val(msg, "router")
// 	mid := util.Val(msg, "mid")
// 	//cand := msg["trickle"]
// 	r := rtc.GetOrNewRouter(router)
// 	t := r.GetSub(mid)
// 	if t != nil {
// 		//t.(*transport.WebRTCTransport).AddCandidate(cand)
// 	}

// 	return nil, util.NewNpError(404, "trickle: WebRTCTransport not found!")
// }