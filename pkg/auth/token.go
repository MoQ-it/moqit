// Package auth issues and verifies moqit access tokens: HS256 JWTs signed
// with an API secret, whose issuer is the API key.
package auth

import (
	"errors"
	"fmt"
	"slices"
	"time"

	moqitv1 "github.com/MoQ-it/moqit/proto/moqit/v1"
	"github.com/golang-jwt/jwt/v5"
)

// RoomGrant says what a token holder may do with rooms.
type RoomGrant struct {
	// Join lets the holder join Room as a participant.
	Join bool   `json:"join,omitempty"`
	Room string `json:"room,omitempty"`
	// Admin API permissions.
	Create bool `json:"create,omitempty"`
	List   bool `json:"list,omitempty"`
	Admin  bool `json:"admin,omitempty"`

	// Participant permissions. Publish, Subscribe and PublishData default
	// to true when absent.
	Publish           *bool    `json:"publish,omitempty"`
	Subscribe         *bool    `json:"subscribe,omitempty"`
	PublishData       *bool    `json:"publishData,omitempty"`
	PublishSources    []string `json:"publishSources,omitempty"` // camera, microphone, screen, screen-audio; empty = all
	UpdateOwnMetadata bool     `json:"updateOwnMetadata,omitempty"`
	Hidden            bool     `json:"hidden,omitempty"`

	// Viewer lets the holder watch Room read-only: it may subscribe to the
	// room track and to participants' tracks, but never publishes and is
	// not a participant. It takes precedence over Join.
	Viewer bool `json:"viewer,omitempty"`
}

// Grants are the permissions in a token.
type Grants struct {
	Room *RoomGrant `json:"room,omitempty"`
	// Server marks moqit's own sessions (the room service's loopback
	// session, other cluster nodes). Never issue it to clients.
	Server bool `json:"server,omitempty"`
}

// Claims is the JWT payload.
type Claims struct {
	jwt.RegisteredClaims
	Name       string            `json:"name,omitempty"`
	Metadata   string            `json:"metadata,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
	Grants     Grants            `json:"grants"`
	// Sha256 is the base64 SHA-256 of a signed request body (webhooks).
	Sha256 string `json:"sha256,omitempty"`
}

// Identity is the participant identity (the JWT subject).
func (c *Claims) Identity() string { return c.Subject }

// IsViewer reports whether the token grants read-only viewing of a room.
func (c *Claims) IsViewer() bool {
	return c.Grants.Room != nil && c.Grants.Room.Viewer && c.Grants.Room.Room != ""
}

// Source names used in PublishSources.
var sourceNames = map[string]moqitv1.TrackSource{
	"camera":       moqitv1.TrackSource_TRACK_SOURCE_CAMERA,
	"microphone":   moqitv1.TrackSource_TRACK_SOURCE_MICROPHONE,
	"screen":       moqitv1.TrackSource_TRACK_SOURCE_SCREEN,
	"screen-audio": moqitv1.TrackSource_TRACK_SOURCE_SCREEN_AUDIO,
}

func orTrue(b *bool) bool { return b == nil || *b }

// Permission converts a room grant into participant permissions.
func (g *RoomGrant) Permission() (*moqitv1.ParticipantPermission, error) {
	p := &moqitv1.ParticipantPermission{
		CanSubscribe:      orTrue(g.Subscribe),
		CanPublish:        orTrue(g.Publish),
		CanPublishData:    orTrue(g.PublishData),
		CanUpdateMetadata: g.UpdateOwnMetadata,
		Hidden:            g.Hidden,
	}
	for _, name := range g.PublishSources {
		src, ok := sourceNames[name]
		if !ok {
			return nil, fmt.Errorf("unknown publish source %q", name)
		}
		p.CanPublishSources = append(p.CanPublishSources, src)
	}
	return p, nil
}

// CanPublishSource reports whether a permission allows publishing a source.
func CanPublishSource(p *moqitv1.ParticipantPermission, src moqitv1.TrackSource) bool {
	return p.CanPublish && (len(p.CanPublishSources) == 0 || slices.Contains(p.CanPublishSources, src))
}

// AccessToken builds a token.
type AccessToken struct {
	apiKey, secret string
	claims         Claims
	validFor       time.Duration
}

// NewAccessToken starts a token signed with the given API key and secret.
func NewAccessToken(apiKey, secret string) *AccessToken {
	return &AccessToken{apiKey: apiKey, secret: secret, validFor: 6 * time.Hour}
}

func (t *AccessToken) SetIdentity(identity string) *AccessToken {
	t.claims.Subject = identity
	return t
}

func (t *AccessToken) SetName(name string) *AccessToken {
	t.claims.Name = name
	return t
}

func (t *AccessToken) SetMetadata(md string) *AccessToken {
	t.claims.Metadata = md
	return t
}

func (t *AccessToken) SetAttributes(a map[string]string) *AccessToken {
	t.claims.Attributes = a
	return t
}

func (t *AccessToken) SetRoomGrant(g *RoomGrant) *AccessToken {
	t.claims.Grants.Room = g
	return t
}

// SetServerGrant marks an internal token; see Grants.Server.
func (t *AccessToken) SetServerGrant() *AccessToken {
	t.claims.Grants.Server = true
	return t
}

// SetSha256 binds the token to a body (base64 SHA-256), as webhook
// signatures do.
func (t *AccessToken) SetSha256(sum string) *AccessToken {
	t.claims.Sha256 = sum
	return t
}

func (t *AccessToken) SetValidFor(d time.Duration) *AccessToken {
	t.validFor = d
	return t
}

// ToJWT signs the token.
func (t *AccessToken) ToJWT() (string, error) {
	if t.apiKey == "" || t.secret == "" {
		return "", errors.New("auth: API key and secret required")
	}
	if g := t.claims.Grants.Room; g != nil {
		if g.Join && (t.claims.Subject == "" || g.Room == "") {
			return "", errors.New("auth: a join grant needs an identity and a room")
		}
		if g.Viewer && g.Room == "" {
			return "", errors.New("auth: a viewer grant needs a room")
		}
		if _, err := g.Permission(); err != nil {
			return "", err
		}
	}
	now := time.Now()
	c := t.claims
	c.Issuer = t.apiKey
	c.IssuedAt = jwt.NewNumericDate(now)
	c.NotBefore = jwt.NewNumericDate(now)
	c.ExpiresAt = jwt.NewNumericDate(now.Add(t.validFor))
	return jwt.NewWithClaims(jwt.SigningMethodHS256, &c).SignedString([]byte(t.secret))
}

// KeyProvider maps API keys to secrets.
type KeyProvider map[string]string

// ErrInvalidToken is wrapped by every verification failure.
var ErrInvalidToken = errors.New("auth: invalid token")

// Verify checks a token's signature, issuer and validity window.
func (kp KeyProvider) Verify(token string) (*Claims, error) {
	var c Claims
	_, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		iss, err := t.Claims.GetIssuer()
		if err != nil {
			return nil, err
		}
		secret, ok := kp[iss]
		if !ok {
			return nil, fmt.Errorf("unknown API key %q", iss)
		}
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithLeeway(30*time.Second), jwt.WithExpirationRequired())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	return &c, nil
}
