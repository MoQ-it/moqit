// Package service implements moqit's server API (RoomService) over Connect.
package service

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"connectrpc.com/connect"
	"github.com/MoQ-it/moqit/pkg/auth"
	"github.com/MoQ-it/moqit/pkg/room"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/MoQ-it/moqit/proto/moqit/v1/moqitv1connect"
)

// RoomService implements moqitv1connect.RoomServiceHandler.
type RoomService struct {
	m    *room.Manager
	keys auth.KeyProvider
}

// NewRoomService returns the API's path and handler.
func NewRoomService(m *room.Manager, keys auth.KeyProvider) (string, http.Handler) {
	return moqitv1connect.NewRoomServiceHandler(&RoomService{m: m, keys: keys})
}

var roomName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

type need int

const (
	needCreate need = iota
	needList
	needAdmin
)

// authorize checks the request's bearer token for the grant an operation
// needs. An admin grant applies to its room, or to every room when it names
// none.
func (s *RoomService) authorize(h http.Header, n need, rm string) error {
	tok, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("missing bearer token"))
	}
	c, err := s.keys.Verify(tok)
	if err != nil {
		return connect.NewError(connect.CodeUnauthenticated, err)
	}
	g := c.Grants.Room
	allowed := g != nil && (n == needCreate && g.Create || n == needList && g.List || n == needAdmin && g.Admin && (g.Room == "" || g.Room == rm))
	if !allowed {
		return connect.NewError(connect.CodePermissionDenied, errors.New("token lacks the required grant"))
	}
	if n != needList && !roomName.MatchString(rm) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid room name"))
	}
	return nil
}

func apiError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, room.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

func (s *RoomService) CreateRoom(ctx context.Context, req *connect.Request[moqitv1.CreateRoomRequest]) (*connect.Response[moqitv1.CreateRoomResponse], error) {
	if err := s.authorize(req.Header(), needCreate, req.Msg.Name); err != nil {
		return nil, err
	}
	r, err := s.m.CreateRoom(ctx, req.Msg)
	if err != nil {
		return nil, apiError(err)
	}
	return connect.NewResponse(&moqitv1.CreateRoomResponse{Room: r}), nil
}

func (s *RoomService) ListRooms(ctx context.Context, req *connect.Request[moqitv1.ListRoomsRequest]) (*connect.Response[moqitv1.ListRoomsResponse], error) {
	if err := s.authorize(req.Header(), needList, ""); err != nil {
		return nil, err
	}
	rooms, err := s.m.ListRooms(ctx, req.Msg.Names)
	if err != nil {
		return nil, apiError(err)
	}
	return connect.NewResponse(&moqitv1.ListRoomsResponse{Rooms: rooms}), nil
}

func (s *RoomService) DeleteRoom(ctx context.Context, req *connect.Request[moqitv1.DeleteRoomRequest]) (*connect.Response[moqitv1.DeleteRoomResponse], error) {
	if err := s.authorize(req.Header(), needCreate, req.Msg.Room); err != nil {
		return nil, err
	}
	return connect.NewResponse(&moqitv1.DeleteRoomResponse{}), apiError(s.m.DeleteRoom(ctx, req.Msg.Room))
}

func (s *RoomService) UpdateRoomMetadata(ctx context.Context, req *connect.Request[moqitv1.UpdateRoomMetadataRequest]) (*connect.Response[moqitv1.UpdateRoomMetadataResponse], error) {
	if err := s.authorize(req.Header(), needAdmin, req.Msg.Room); err != nil {
		return nil, err
	}
	r, err := s.m.UpdateRoomMetadata(ctx, req.Msg.Room, req.Msg.Metadata)
	if err != nil {
		return nil, apiError(err)
	}
	return connect.NewResponse(&moqitv1.UpdateRoomMetadataResponse{Room: r}), nil
}

func (s *RoomService) ListParticipants(ctx context.Context, req *connect.Request[moqitv1.ListParticipantsRequest]) (*connect.Response[moqitv1.ListParticipantsResponse], error) {
	if err := s.authorize(req.Header(), needAdmin, req.Msg.Room); err != nil {
		return nil, err
	}
	ps, err := s.m.ListParticipants(ctx, req.Msg.Room)
	if err != nil {
		return nil, apiError(err)
	}
	return connect.NewResponse(&moqitv1.ListParticipantsResponse{Participants: ps}), nil
}

func (s *RoomService) GetParticipant(ctx context.Context, req *connect.Request[moqitv1.GetParticipantRequest]) (*connect.Response[moqitv1.GetParticipantResponse], error) {
	if err := s.authorize(req.Header(), needAdmin, req.Msg.Room); err != nil {
		return nil, err
	}
	p, err := s.m.GetParticipant(ctx, req.Msg.Room, req.Msg.Identity)
	if err != nil {
		return nil, apiError(err)
	}
	return connect.NewResponse(&moqitv1.GetParticipantResponse{Participant: p}), nil
}

func (s *RoomService) RemoveParticipant(ctx context.Context, req *connect.Request[moqitv1.RemoveParticipantRequest]) (*connect.Response[moqitv1.RemoveParticipantResponse], error) {
	if err := s.authorize(req.Header(), needAdmin, req.Msg.Room); err != nil {
		return nil, err
	}
	return connect.NewResponse(&moqitv1.RemoveParticipantResponse{}), apiError(s.m.RemoveParticipant(ctx, req.Msg.Room, req.Msg.Identity))
}

func (s *RoomService) UpdateParticipant(ctx context.Context, req *connect.Request[moqitv1.UpdateParticipantRequest]) (*connect.Response[moqitv1.UpdateParticipantResponse], error) {
	if err := s.authorize(req.Header(), needAdmin, req.Msg.Room); err != nil {
		return nil, err
	}
	p, err := s.m.UpdateParticipant(ctx, req.Msg)
	if err != nil {
		return nil, apiError(err)
	}
	return connect.NewResponse(&moqitv1.UpdateParticipantResponse{Participant: p}), nil
}

func (s *RoomService) MuteTrack(ctx context.Context, req *connect.Request[moqitv1.MuteTrackRequest]) (*connect.Response[moqitv1.MuteTrackResponse], error) {
	if err := s.authorize(req.Header(), needAdmin, req.Msg.Room); err != nil {
		return nil, err
	}
	return connect.NewResponse(&moqitv1.MuteTrackResponse{}), apiError(s.m.MuteTrack(ctx, req.Msg))
}

func (s *RoomService) SendData(ctx context.Context, req *connect.Request[moqitv1.SendDataRequest]) (*connect.Response[moqitv1.SendDataResponse], error) {
	if err := s.authorize(req.Header(), needAdmin, req.Msg.Room); err != nil {
		return nil, err
	}
	if len(req.Msg.Payload) > 64<<10 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("payload over 64 KiB"))
	}
	return connect.NewResponse(&moqitv1.SendDataResponse{}), apiError(s.m.SendData(ctx, req.Msg))
}
