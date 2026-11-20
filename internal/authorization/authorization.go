package authorization

import (
	"bytes"
	"cmp"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/casbin/casbin/v3"
	"github.com/casbin/casbin/v3/model"
	"gopkg.in/yaml.v3"
)

type Principal string
type Action string
type Resource string

const (
	Retrieve Action = "retrieve"

	ZendeskCorpus Resource  = "corpus:zendesk"
	MetricEval    Principal = "service:metric-eval"
)

const aclModel = `[request_definition]
r = principal, action, resource
[policy_definition]
p = principal, action, resource
[policy_effect]
e = some(where (p.eft == allow))
[matchers]
m = r.principal == p.principal && r.action == p.action && r.resource == p.resource
`

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var channelID = regexp.MustCompile(`^[CG][A-Z0-9]+$`)

type Permission struct {
	Action   Action   `yaml:"action" json:"action"`
	Resource Resource `yaml:"resource" json:"resource"`
}

type Grant struct {
	Principals  []Principal  `yaml:"principals"`
	Permissions []Permission `yaml:"permissions"`
}

type Config struct {
	Grants              []Grant                       `yaml:"grants"`
	ChannelRestrictions map[string]ChannelRestriction `yaml:"channel_restrictions"`
	ChannelProfiles     map[string]ChannelProfile     `yaml:"channel_profiles"`
}

type ChannelRestriction struct {
	AllowedScopes []string `yaml:"allowed_scopes"`
	AllowPublic   bool     `yaml:"allow_public"`
}

type ChannelAccess struct {
	AllowedScopes []string
	CorpusOnly    bool
}

type Decision struct {
	permissions []Permission
}

func (decision Decision) Allows(action Action, resource Resource) bool {
	return slices.Contains(decision.permissions, Permission{Action: action, Resource: resource})
}

func (decision Decision) Permissions() []Permission {
	return slices.Clone(decision.permissions)
}

func (decision Decision) CorpusScopes() []string {
	var scopes []string
	for _, permission := range decision.permissions {
		if permission.Action == Retrieve {
			scopes = append(scopes, string(permission.Resource))
		}
	}
	slices.Sort(scopes)
	return scopes
}

// GroupPrincipal names a member of an IdP group, whatever the IdP.
func GroupPrincipal(group string) Principal {
	return Principal("group:" + group)
}

func ChannelPrincipal(channel string) Principal {
	return Principal("slack-channel:" + channel)
}

type policyEnforcer interface {
	Enforce(...interface{}) (bool, error)
}

type Authorizer struct {
	enforcer            policyEnforcer
	permissions         []Permission
	channelRestrictions map[string]ChannelRestriction
}

func Load(path string) (*Authorizer, error) {
	if path == "" {
		return New(Config{})
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("authorization: read policy: %w", err)
	}
	return Parse(data)
}

func Parse(data []byte) (*Authorizer, error) {
	var document struct {
		Authorization *Config `yaml:"authorization"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("authorization: decode policy: %w", err)
	}
	if document.Authorization == nil {
		return nil, fmt.Errorf("authorization: policy must contain an authorization block")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("authorization: decode trailing policy content: %w", err)
		}
		return nil, fmt.Errorf("authorization: policy must contain exactly one YAML document")
	}
	return New(*document.Authorization)
}

func New(config Config) (*Authorizer, error) {
	config, err := expandChannelProfiles(config)
	if err != nil {
		return nil, err
	}
	restrictions := make(map[string]ChannelRestriction, len(config.ChannelRestrictions))
	for channel, restriction := range config.ChannelRestrictions {
		if !channelID.MatchString(channel) {
			return nil, fmt.Errorf("authorization: invalid restricted channel %q", channel)
		}
		if len(restriction.AllowedScopes) == 0 {
			return nil, fmt.Errorf("authorization: restricted channel %q requires allowed_scopes", channel)
		}
		for _, scope := range restriction.AllowedScopes {
			if !validPermission(Permission{Action: Retrieve, Resource: Resource(scope)}) {
				return nil, fmt.Errorf("authorization: restricted channel %q has invalid resource scope %q", channel, scope)
			}
		}
		restriction.AllowedScopes = slices.Clone(restriction.AllowedScopes)
		restrictions[channel] = restriction
	}
	rows := make([][]string, 0)
	seenRows := make(map[[3]string]struct{})
	seenPermissions := make(map[Permission]struct{})
	for grantIndex, grant := range config.Grants {
		if len(grant.Principals) == 0 || len(grant.Permissions) == 0 {
			return nil, fmt.Errorf("authorization: grant %d requires principals and permissions", grantIndex)
		}
		for _, principal := range grant.Principals {
			if !validPrincipal(principal) {
				return nil, fmt.Errorf("authorization: grant %d has invalid principal %q", grantIndex, principal)
			}
		}
		for _, permission := range grant.Permissions {
			if !validPermission(permission) {
				return nil, fmt.Errorf("authorization: grant %d has invalid permission %q on %q", grantIndex, permission.Action, permission.Resource)
			}
			seenPermissions[permission] = struct{}{}
			for _, principal := range grant.Principals {
				row := [3]string{string(principal), string(permission.Action), string(permission.Resource)}
				if _, exists := seenRows[row]; !exists {
					seenRows[row] = struct{}{}
					rows = append(rows, []string{row[0], row[1], row[2]})
				}
			}
		}
	}
	parsedModel, err := model.NewModelFromString(aclModel)
	if err != nil {
		return nil, fmt.Errorf("authorization: construct model: %w", err)
	}
	enforcer, err := casbin.NewEnforcer(parsedModel)
	if err != nil {
		return nil, fmt.Errorf("authorization: construct enforcer: %w", err)
	}
	if len(rows) > 0 {
		if _, err := enforcer.AddPolicies(rows); err != nil {
			return nil, fmt.Errorf("authorization: load grants: %w", err)
		}
	}
	permissions := make([]Permission, 0, len(seenPermissions))
	for permission := range seenPermissions {
		permissions = append(permissions, permission)
	}
	slices.SortFunc(permissions, func(left, right Permission) int {
		if order := cmp.Compare(left.Action, right.Action); order != 0 {
			return order
		}
		return cmp.Compare(left.Resource, right.Resource)
	})
	return &Authorizer{enforcer: enforcer, permissions: permissions, channelRestrictions: restrictions}, nil
}

func (authorizer *Authorizer) RestrictChannelScopes(channel string, scopes []string) ChannelAccess {
	if authorizer == nil {
		return ChannelAccess{}
	}
	access := ChannelAccess{AllowedScopes: slices.Clone(scopes)}
	restriction, restricted := authorizer.channelRestrictions[channel]
	if !restricted {
		return access
	}
	access.CorpusOnly = true
	access.AllowedScopes = slices.DeleteFunc(access.AllowedScopes, func(scope string) bool {
		return scope != "public" && !slices.Contains(restriction.AllowedScopes, scope)
	})
	return access
}

func (authorizer *Authorizer) AllowsPublicChannel(channel string) bool {
	return authorizer != nil && authorizer.channelRestrictions[channel].AllowPublic
}

func (authorizer *Authorizer) Resolve(principals []Principal) (Decision, error) {
	if authorizer == nil || authorizer.enforcer == nil {
		return Decision{}, nil
	}
	var decision Decision
	for _, permission := range authorizer.permissions {
		for _, principal := range principals {
			allowed, err := authorizer.enforcer.Enforce(string(principal), string(permission.Action), string(permission.Resource))
			if err != nil {
				return Decision{}, fmt.Errorf("authorization: evaluate %q on %q: %w", permission.Action, permission.Resource, err)
			}
			if allowed {
				decision.permissions = append(decision.permissions, permission)
				break
			}
		}
	}
	return decision, nil
}

func validPrincipal(principal Principal) bool {
	kind, name, found := strings.Cut(string(principal), ":")
	if !found {
		return false
	}
	switch kind {
	case "group", "service":
		return identifier.MatchString(name)
	case "slack-channel":
		return channelID.MatchString(name)
	default:
		return false
	}
}

func validPermission(permission Permission) bool {
	kind, name, found := strings.Cut(string(permission.Resource), ":")
	if !found || !identifier.MatchString(name) {
		return false
	}
	return permission.Action == Retrieve && (kind == "corpus" || (kind == "slack-channel" && channelID.MatchString(name)))
}
