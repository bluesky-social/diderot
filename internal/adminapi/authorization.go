package adminapi

import (
	"context"
	"net/http"

	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/authorization"
)

func (s *Server) SetAuthorizer(authorizer *authorization.Authorizer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authorizer = authorizer
}

func (s *Server) authorizationForGroups(ctx context.Context, groups []string) authorization.Decision {
	s.mu.Lock()
	authorizer := s.authorizer
	s.mu.Unlock()
	principals := make([]authorization.Principal, 0, len(groups))
	for _, group := range groups {
		principals = append(principals, authorization.GroupPrincipal(group))
	}
	decision, err := authorizer.Resolve(principals)
	if err != nil {
		s.logger.WarnContext(ctx, "protected authorization failed", "err", err)
		return authorization.Decision{}
	}
	return decision
}

func (s *Server) authorizationForRequest(r *http.Request) []string {
	return s.scopesForPrincipal(r.Context(), operatorFromContext(r.Context()))
}

// scopesForPrincipal is what a caller may read: the scopes of their email,
// and those their groups are granted. A nil principal is "no identity" and
// takes the same path as an explicit empty one (&auth.Principal{}): empty
// email, no groups.
func (s *Server) scopesForPrincipal(ctx context.Context, principal *auth.Principal) []string {
	if principal == nil {
		principal = &auth.Principal{}
	}
	decision := s.authorizationForGroups(ctx, principal.Groups)
	scopes := append([]string(nil), s.scopesForEmail(ctx, principal.Email)...)
	return s.grantScopes(ctx, append(scopes, decision.CorpusScopes()...))
}

// grantScopes adds the scopes sources grant to a reader holding scopes.
func (s *Server) grantScopes(ctx context.Context, scopes []string) []string {
	s.mu.Lock()
	grants := s.scopeGrants
	s.mu.Unlock()
	if grants == nil {
		return scopes
	}
	return grants.GrantScopes(ctx, scopes)
}
