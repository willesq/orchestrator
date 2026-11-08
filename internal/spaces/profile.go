package spaces

import (
	"slices"
	"sort"
)

// GHProfile is the gh login a space's GitHub calls run with (D-18). ConfigDir
// goes to gh as GH_CONFIG_DIR; orch never reads inside it.
type GHProfile struct {
	Space string
	Kind  Kind
	// ConfigDir is the space's gh_config_dir, expanded; "" is the default
	// gh login.
	ConfigDir string
	// Orgs are the GitHub orgs the space binds.
	Orgs []string
}

// Required reports whether the space's GitHub calls must run with its own
// profile: an isolated space's never fall back to the default login.
func (p GHProfile) Required() bool { return p.Kind == Isolated }

// GHProfileFor returns the profile for GitHub calls about account login: that
// of the space binding it. ok is false when no space binds login.
func (r *Resolved) GHProfileFor(login string) (GHProfile, bool) {
	space, ok := r.GitHubOrgSpace(login)
	if !ok {
		return GHProfile{}, false
	}
	return r.ghProfile(space), true
}

// GHProfiles lists, by space name, the profile of every space that binds a
// GitHub org or names a gh_config_dir.
func (r *Resolved) GHProfiles() []GHProfile {
	if !r.Configured() {
		return nil
	}
	var out []GHProfile
	for name, s := range r.spaces {
		if len(s.GitHubOrgs) > 0 || r.ghDirs[name] != "" {
			out = append(out, r.ghProfile(name))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Space < out[j].Space })
	return out
}

func (r *Resolved) ghProfile(name string) GHProfile {
	s := r.spaces[name]
	return GHProfile{Space: name, Kind: s.Kind, ConfigDir: r.ghDirs[name], Orgs: slices.Clone(s.GitHubOrgs)}
}
