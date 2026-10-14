// Package moqit is the Go server SDK for moqit: access tokens, the
// RoomService API client and webhook verification.
//
//	tok, _ := moqit.NewAccessToken(key, secret).SetIdentity("alice").
//		SetRoomGrant(&moqit.RoomGrant{Join: true, Room: "standup"}).ToJWT()
//
//	rooms := moqit.NewRoomServiceClient("https://moqit.example.com:7880", key, secret)
//	res, err := rooms.ListParticipants(ctx, connect.NewRequest(&moqitv1.ListParticipantsRequest{Room: "standup"}))
//
//	http.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
//		ev, err := moqit.ReceiveWebhook(r, moqit.KeyProvider{key: secret})
//		...
//	})
package moqit

import (
	"context"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/MoQ-it/moqit/pkg/auth"
	"github.com/MoQ-it/moqit/pkg/webhook"
	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/MoQ-it/moqit/proto/moqit/v1/moqitv1connect"
)

type (
	AccessToken = auth.AccessToken
	RoomGrant   = auth.RoomGrant
	Claims      = auth.Claims
	KeyProvider = auth.KeyProvider
)

// NewAccessToken starts a token signed with an API key and secret.
func NewAccessToken(apiKey, secret string) *AccessToken { return auth.NewAccessToken(apiKey, secret) }

// NewRoomServiceClient returns a RoomService client for a node's HTTP API
// (e.g. http://localhost:7880). Each call carries a short-lived token with
// create, list and admin grants for every room, signed with the API key.
func NewRoomServiceClient(baseURL, apiKey, secret string, opts ...connect.ClientOption) moqitv1connect.RoomServiceClient {
	opts = append([]connect.ClientOption{connect.WithInterceptors(authInterceptor(apiKey, secret))}, opts...)
	return moqitv1connect.NewRoomServiceClient(http.DefaultClient, baseURL, opts...)
}

func authInterceptor(apiKey, secret string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			tok, err := auth.NewAccessToken(apiKey, secret).
				SetRoomGrant(&auth.RoomGrant{Create: true, List: true, Admin: true}).
				SetValidFor(10 * time.Minute).ToJWT()
			if err != nil {
				return nil, err
			}
			req.Header().Set("Authorization", "Bearer "+tok)
			return next(ctx, req)
		}
	}
}

// ReceiveWebhook reads and verifies a webhook request.
func ReceiveWebhook(r *http.Request, keys KeyProvider) (*moqitv1.WebhookEvent, error) {
	return webhook.Receive(r, keys)
}
