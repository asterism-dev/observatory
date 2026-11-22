// Package iamtest is an in-memory iam for other services' tests: JWKS, the token exchange, the policy stream and IAMInternalService.
package iamtest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	coreauthz "github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/authz"
	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
)

// Issuer is the iss of every JWT the fake mints; verifiers must expect it.
const Issuer = "https://observatory.test"

const sessionTTL = time.Hour

type User struct {
	ID          uuid.UUID
	Name, Email string
}

type session struct {
	user    uuid.UUID
	kind    string
	token   string
	expires time.Time
	revoked bool
}

type Server struct {
	t      testing.TB
	srv    *httptest.Server
	token  string
	signer *identity.Signer
	pub    *authz.Publisher

	mu       sync.Mutex
	pol      coreauthz.Policy
	users    map[uuid.UUID]User
	admins   map[uuid.UUID]bool
	personal map[uuid.UUID]uuid.UUID
	teams    map[uuid.UUID]string
	keys     map[uuid.UUID][]string
	sessions map[uuid.UUID]*session
}

// New starts the fake; it stops when t ends.
func New(t testing.TB) *Server {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := identity.NewSigner(key, Issuer)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		t:        t,
		token:    rand.Text() + rand.Text(),
		signer:   signer,
		pub:      authz.NewPublisher(),
		users:    map[uuid.UUID]User{},
		admins:   map[uuid.UUID]bool{},
		personal: map[uuid.UUID]uuid.UUID{},
		teams:    map[uuid.UUID]string{},
		keys:     map[uuid.UUID][]string{},
		sessions: map[uuid.UUID]*session{},
	}
	s.pub.Publish(s.pol)
	mux := http.NewServeMux()
	mux.Handle("/internal/iam/jwks", s.signer.JWKSHandler())
	mux.HandleFunc("/internal/iam/token", s.exchange)
	path, h := iamv1connect.NewIAMInternalServiceHandler(internalAPI{s})
	mux.Handle("/internal/iam"+path, http.StripPrefix("/internal/iam", h))
	mux.Handle(s.pub.Handler("/internal/iam"))
	s.srv = httptest.NewServer(servicetoken.Middleware(map[string]string{"test": s.token})(mux))
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the base callers append /internal/iam/... to.
func (s *Server) URL() string { return s.srv.URL }

// ServiceToken is the bearer every call to the fake needs.
func (s *Server) ServiceToken() string { return s.token }

// Client calls the fake's IAMInternalService with its service token.
func (s *Server) Client() iamv1connect.IAMInternalServiceClient {
	return iamv1connect.NewIAMInternalServiceClient(s.srv.Client(), s.srv.URL+"/internal/iam", connect.WithInterceptors(servicetoken.Bearer(s.token)))
}

// Revision is the latest published policy revision.
func (s *Server) Revision() uint64 { return s.pub.Revision() }

// change applies fn and publishes the policy, as every iam commit does.
func (s *Server) change(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn()
	s.pub.Publish(s.pol)
}

// AddUser creates a user with a personal org it owns; the first user is a platform admin, as in iam.
func (s *Server) AddUser(name string) User {
	u := User{ID: uuid.New(), Name: name, Email: name + "@x.io"}
	org := uuid.New()
	s.change(func() {
		s.admins[u.ID] = len(s.users) == 0
		s.users[u.ID] = u
		s.personal[u.ID] = org
		s.pol.Memberships = append(s.pol.Memberships, coreauthz.MemberRole{OrgID: org, UserID: u.ID, Role: "owner"})
	})
	return u
}

func (s *Server) PersonalOrg(user uuid.UUID) uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.personal[user]
}

// CreateOrg creates an org owned by owner; the fake keeps no org names.
func (s *Server) CreateOrg(owner uuid.UUID, _ string) uuid.UUID {
	org := uuid.New()
	s.AddMember(org, owner, "owner")
	return org
}

// AddMember adds user to org, or changes the member's role.
func (s *Server) AddMember(org, user uuid.UUID, role string) {
	s.change(func() {
		s.pol.Memberships = slices.DeleteFunc(s.pol.Memberships, func(m coreauthz.MemberRole) bool { return m.OrgID == org && m.UserID == user })
		s.pol.Memberships = append(s.pol.Memberships, coreauthz.MemberRole{OrgID: org, UserID: user, Role: role})
	})
}

// RemoveMember drops the membership with the user's team memberships and bindings in org, as iam does.
func (s *Server) RemoveMember(org, user uuid.UUID) {
	subject := coreauthz.UserSubject(user)
	s.change(func() {
		s.pol.Memberships = slices.DeleteFunc(s.pol.Memberships, func(m coreauthz.MemberRole) bool { return m.OrgID == org && m.UserID == user })
		s.pol.TeamMembers = slices.DeleteFunc(s.pol.TeamMembers, func(m coreauthz.TeamMember) bool { return m.OrgID == org && m.UserID == user })
		s.pol.Bindings = slices.DeleteFunc(s.pol.Bindings, func(b coreauthz.Binding) bool { return b.OrgID == org && b.Subject == subject })
	})
}

func (s *Server) AddTeam(org uuid.UUID, name string) uuid.UUID {
	team := uuid.New()
	s.change(func() {
		s.teams[team] = name
		s.pol.Teams = append(s.pol.Teams, coreauthz.TeamRef{OrgID: org, ID: team})
	})
	return team
}

// RemoveTeam drops the team with its members and bindings, as iam does.
func (s *Server) RemoveTeam(org, team uuid.UUID) {
	subject := coreauthz.TeamSubject(team)
	s.change(func() {
		delete(s.teams, team)
		s.pol.Teams = slices.DeleteFunc(s.pol.Teams, func(t coreauthz.TeamRef) bool { return t.ID == team })
		s.pol.TeamMembers = slices.DeleteFunc(s.pol.TeamMembers, func(m coreauthz.TeamMember) bool { return m.TeamID == team })
		s.pol.Bindings = slices.DeleteFunc(s.pol.Bindings, func(b coreauthz.Binding) bool { return b.OrgID == org && b.Subject == subject })
	})
}

// SetTeamMember adds user to team, or changes the member's admin flag.
func (s *Server) SetTeamMember(org, team, user uuid.UUID, admin bool) {
	s.change(func() {
		s.pol.TeamMembers = slices.DeleteFunc(s.pol.TeamMembers, func(m coreauthz.TeamMember) bool { return m.TeamID == team && m.UserID == user })
		s.pol.TeamMembers = append(s.pol.TeamMembers, coreauthz.TeamMember{OrgID: org, TeamID: team, UserID: user, Admin: admin})
	})
}

func (s *Server) RemoveTeamMember(org, team, user uuid.UUID) {
	s.change(func() {
		s.pol.TeamMembers = slices.DeleteFunc(s.pol.TeamMembers, func(m coreauthz.TeamMember) bool {
			return m.OrgID == org && m.TeamID == team && m.UserID == user
		})
	})
}

// Bind binds subject to role in org, creating the role or replacing the one with role.ID.
func (s *Server) Bind(org uuid.UUID, subject string, role coreauthz.CustomRole) {
	if role.ID == uuid.Nil {
		role.ID = uuid.New()
	}
	role.OrgID = org
	s.change(func() {
		s.pol.Roles = slices.DeleteFunc(s.pol.Roles, func(r coreauthz.CustomRole) bool { return r.ID == role.ID })
		s.pol.Roles = append(s.pol.Roles, role)
		s.pol.Bindings = append(s.pol.Bindings, coreauthz.Binding{OrgID: org, Subject: subject, RoleID: role.ID})
	})
}

// AddSSHKey publishes nothing new, since keys are not in the policy.
func (s *Server) AddSSHKey(user uuid.UUID, key string) {
	s.change(func() { s.keys[user] = append(s.keys[user], key) })
}

func (s *Server) RevokeSession(id uuid.UUID) {
	s.change(func() {
		if sess, ok := s.sessions[id]; ok {
			sess.revoked = true
		}
	})
}

// SessionToken opens a session and returns the token the edge exchanges for a JWT.
func (s *Server) SessionToken(user uuid.UUID, kind string) (string, uuid.UUID) {
	id := uuid.New()
	tok := "obs_" + rand.Text()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = &session{user: user, kind: kind, token: tok, expires: time.Now().Add(sessionTTL)}
	return tok, id
}

// JWT opens a session of kind ("browser" or "cli") for user and returns a JWT for it.
func (s *Server) JWT(user uuid.UUID, kind string) (string, uuid.UUID) {
	_, id := s.SessionToken(user, kind)
	s.mu.Lock()
	jwt, _, err := s.mint(id)
	s.mu.Unlock()
	if err != nil {
		s.t.Fatalf("iamtest: mint: %v", err)
	}
	return jwt, id
}

// mint needs s.mu held.
func (s *Server) mint(id uuid.UUID) (string, time.Time, error) {
	sess := s.sessions[id]
	u, ok := s.users[sess.user]
	if !ok {
		return "", time.Time{}, fmt.Errorf("iamtest: unknown user %s", sess.user)
	}
	return s.signer.Mint(identity.Principal{User: u.ID, Session: id, PlatformAdmin: s.admins[u.ID], Name: u.Name, SessionKind: sess.kind, SessionExpires: sess.expires})
}

// exchange is iam's token endpoint: a live session token becomes a JWT.
func (s *Server) exchange(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SessionToken string `json:"session_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.sessions {
		if in.SessionToken == "" || sess.token != in.SessionToken || sess.revoked || !sess.expires.After(time.Now()) {
			continue
		}
		jwt, exp, err := s.mint(id)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"token": jwt, "expires_at": exp})
		return
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// internalAPI serves IAMInternalService from the fake's state, with iam's answers for unknown ids.
type internalAPI struct{ s *Server }

func parseID(field, raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid "+field))
	}
	return id, nil
}

func (a internalAPI) GetUsers(_ context.Context, req *connect.Request[iamv1.GetUsersRequest]) (*connect.Response[iamv1.GetUsersResponse], error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	out := &iamv1.GetUsersResponse{}
	for _, raw := range req.Msg.UserIds {
		id, err := parseID("user_id", raw)
		if err != nil {
			return nil, err
		}
		if u, ok := a.s.users[id]; ok {
			out.Users = append(out.Users, &iamv1.User{Id: u.ID.String(), Name: u.Name, Email: u.Email})
		}
	}
	return connect.NewResponse(out), nil
}

func (a internalAPI) ListSSHKeys(_ context.Context, req *connect.Request[iamv1.ListSSHKeysRequest]) (*connect.Response[iamv1.ListSSHKeysResponse], error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	out := &iamv1.ListSSHKeysResponse{}
	for _, raw := range req.Msg.UserIds {
		id, err := parseID("user_id", raw)
		if err != nil {
			return nil, err
		}
		for _, k := range a.s.keys[id] {
			out.Keys = append(out.Keys, &iamv1.SSHKey{UserId: id.String(), PublicKey: k})
		}
	}
	return connect.NewResponse(out), nil
}

func (a internalAPI) ListTeams(_ context.Context, req *connect.Request[iamv1.ListTeamsRequest]) (*connect.Response[iamv1.ListTeamsResponse], error) {
	org, err := parseID("org_id", req.Msg.OrgId)
	if err != nil {
		return nil, err
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	out := &iamv1.ListTeamsResponse{}
	for _, t := range a.s.pol.Teams {
		if t.OrgID == org {
			out.Teams = append(out.Teams, &iamv1.Team{Id: t.ID.String(), Name: a.s.teams[t.ID]})
		}
	}
	return connect.NewResponse(out), nil
}

func (a internalAPI) GetMembership(_ context.Context, req *connect.Request[iamv1.GetMembershipRequest]) (*connect.Response[iamv1.GetMembershipResponse], error) {
	org, err := parseID("org_id", req.Msg.OrgId)
	if err != nil {
		return nil, err
	}
	user, err := parseID("user_id", req.Msg.UserId)
	if err != nil {
		return nil, err
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	for _, m := range a.s.pol.Memberships {
		if m.OrgID == org && m.UserID == user {
			return connect.NewResponse(&iamv1.GetMembershipResponse{Member: true, Role: m.Role}), nil
		}
	}
	return connect.NewResponse(&iamv1.GetMembershipResponse{}), nil
}

func (a internalAPI) GetSession(_ context.Context, req *connect.Request[iamv1.GetSessionRequest]) (*connect.Response[iamv1.GetSessionResponse], error) {
	id, err := parseID("session_id", req.Msg.SessionId)
	if err != nil {
		return nil, err
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	sess, ok := a.s.sessions[id]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such session"))
	}
	return connect.NewResponse(&iamv1.GetSessionResponse{UserId: sess.user.String(), ExpiresAt: timestamppb.New(sess.expires), Revoked: sess.revoked, Expired: !sess.expires.After(time.Now())}), nil
}
