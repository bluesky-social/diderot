package authorization

import (
	"fmt"
	"maps"
	"slices"
)

type ChannelProfile struct {
	Channels          []string     `yaml:"channels"`
	Permissions       []Permission `yaml:"permissions"`
	AllowedScopes     []string     `yaml:"allowed_scopes"`
	IncludeOwnChannel bool         `yaml:"include_own_channel"`
	AllowPublic       bool         `yaml:"allow_public"`
}

func expandChannelProfiles(config Config) (Config, error) {
	config.Grants = slices.Clone(config.Grants)
	config.ChannelRestrictions = maps.Clone(config.ChannelRestrictions)
	if config.ChannelRestrictions == nil {
		config.ChannelRestrictions = make(map[string]ChannelRestriction)
	}
	for _, name := range slices.Sorted(maps.Keys(config.ChannelProfiles)) {
		profile := config.ChannelProfiles[name]
		if !identifier.MatchString(name) || len(profile.Channels) == 0 {
			return Config{}, fmt.Errorf("authorization: channel profile %q requires a valid name and channels", name)
		}
		for _, permission := range profile.Permissions {
			if !validPermission(permission) || !slices.Contains(profile.AllowedScopes, string(permission.Resource)) {
				return Config{}, fmt.Errorf("authorization: channel profile %q has invalid permission or resource outside allowed_scopes: %q on %q", name, permission.Action, permission.Resource)
			}
		}
		principals := make([]Principal, 0, len(profile.Channels))
		for _, channel := range profile.Channels {
			if _, exists := config.ChannelRestrictions[channel]; exists {
				return Config{}, fmt.Errorf("authorization: channel %q has multiple restrictions or profile assignments", channel)
			}
			scopes := slices.Clone(profile.AllowedScopes)
			if profile.IncludeOwnChannel {
				ownScope := string(ChannelPrincipal(channel))
				if !slices.Contains(scopes, ownScope) {
					scopes = append(scopes, ownScope)
				}
			}
			config.ChannelRestrictions[channel] = ChannelRestriction{AllowedScopes: scopes, AllowPublic: profile.AllowPublic}
			principals = append(principals, ChannelPrincipal(channel))
		}
		if len(profile.Permissions) > 0 {
			config.Grants = append(config.Grants, Grant{Principals: principals, Permissions: slices.Clone(profile.Permissions)})
		}
	}
	return config, nil
}
