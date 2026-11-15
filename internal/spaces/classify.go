package spaces

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// System names the kind of writer an origin comes from.
type System string

const (
	// SystemFS is a local file or checkout: repo sources and edit hand-offs.
	SystemFS System = "fs"
	// SystemGitHub is a docs sync of a GitHub repo.
	SystemGitHub System = "github"
	// SystemWeb is a web page.
	SystemWeb System = "web"
	// SystemSession is a session's own record: transcript or summary.
	SystemSession System = "session"
	// SystemClient is a note a client ingested.
	SystemClient System = "client"
	SystemSlack  System = "slack"
	SystemTeams  System = "teams"
)

// Visibility is a GitHub repo's visibility as GitHub reports it.
type Visibility string

const (
	VisibilityUnknown Visibility = ""
	VisibilityPublic  Visibility = "public"
	VisibilityPrivate Visibility = "private"
)

// Repo is the GitHub repo content belongs to: a checkout's origin remote or
// a docs sync. Owner is the account that owns it now, as GitHub reports it,
// so a transferred repo is placed by its new owner. A fork's parent is never
// recorded: the fork's owner decides.
type Repo struct {
	Owner      string
	Name       string
	Visibility Visibility
	// Unchecked says GitHub gave no answer about the repo: it was not
	// asked (the lookup budget was spent, or only cached answers were
	// allowed) or could not be reached. Visibility is then unknown, so the
	// repo is not placed public, but a public copy is not taken out on
	// that alone (RC-402). A repo GitHub refuses to show is checked.
	Unchecked bool
}

// Origin is the facts a writer reports about content. Writers report facts,
// never a space; Classify decides.
type Origin struct {
	System    System
	SourceURI string
	// Path is a local folder or file: a checkout, a hand-off's file, or for
	// a session record the session's first cwd. It must be absolute.
	Path string
	// IsDir says Path is a folder, not a file: public_paths then decide for
	// the files directly in it. Writes always name a file; an explanation
	// or a checkout's rules may name a folder.
	IsDir  bool
	Remote *Repo
	URL    string
	// Tenant is a chat tenant's stable ID, without its provider.
	Tenant string
	// WriterSpace is where the pinned session that wrote the content writes,
	// as orchd recorded at its pin, for a web page or note a session
	// ingested. "" means no pinned session wrote it.
	WriterSpace string
}

// Options are what Classify reads besides the origin and the policy.
type Options struct {
	// Override is the space a sticky retag of the origin's source_uri names,
	// or "" for none. It wins over every rule.
	Override string
	// Confirmed reports whether a bound GitHub org's identity is confirmed.
	// nil confirms none, so an org's content is placed only when orch.db
	// says its binding holds.
	Confirmed func(login string) bool
}

// Outcome is what Classify decided.
type Outcome string

const (
	Placed      Outcome = "placed"
	Quarantined Outcome = "quarantined"
	// Conflict: two rules name different spaces neither of which is
	// stricter. Nothing is indexed, and a session pinned there has no space.
	Conflict Outcome = "conflict"
)

// PlaceRule names the rule that decided a placement.
type PlaceRule string

const (
	PlaceNoPolicy          PlaceRule = "no_policy"
	PlaceOverride          PlaceRule = "override"
	PlaceOverrideUnknown   PlaceRule = "override_unknown_space"
	PlaceRoot              PlaceRule = "root"
	PlaceRemote            PlaceRule = "remote"
	PlaceAgree             PlaceRule = "root_and_remote_agree"
	PlaceRemoteStricter    PlaceRule = "remote_stricter"
	PlaceRootStricter      PlaceRule = "root_stricter"
	PlaceConflict          PlaceRule = "conflict"
	PlaceNotVerifiedPublic PlaceRule = "not_verified_public"
	PlaceNoPublicProof     PlaceRule = "no_public_proof"
	PlacePublicPath        PlaceRule = "public_path"
	PlaceOrgUnconfirmed    PlaceRule = "org_unconfirmed"
	PlaceWebPrefix         PlaceRule = "web_prefix"
	PlaceWriter            PlaceRule = "writer"
	PlaceWriterStricter    PlaceRule = "writer_stricter"
	PlacePrefixStricter    PlaceRule = "prefix_stricter"
	PlaceWriterUnknown     PlaceRule = "writer_unknown"
	PlaceChatTenant        PlaceRule = "chat_tenant"
	PlacePublicRootSession PlaceRule = "public_root_session"
	PlaceUnmatched         PlaceRule = "unmatched"
)

// RootMatch is one bound root that contains an origin's path.
type RootMatch struct {
	Root  string
	Space string
	Kind  Kind
}

// RemoteEffect is what an origin's GitHub repo contributes.
type RemoteEffect struct {
	Repo Repo
	// Space is the space the owner is bound to, "" when unbound.
	Space  string
	Kind   Kind
	Effect string
}

// Placement is where content goes and why.
type Placement struct {
	// Space is a space, Shared when no policy is applied, or Quarantine,
	// which is also the space of a conflict. It is never a public space for
	// unmatched or conflicting origins.
	Space   string
	Outcome Outcome
	Rule    PlaceRule
	Reason  string
	// Roots are every bound root containing the path, shallowest first;
	// the last one is the deepest and is the root that counts.
	Roots  []RootMatch
	Remote *RemoteEffect
	// PublicPath is the public_paths pattern, as written, that matched local
	// content under a public root (SP-076): with rule public_path it made the
	// content public; with another rule something else kept it out.
	PublicPath string
	// ReadAs is set for SystemSession: the space whose reads a session
	// pinned here gets. Under a public root it is that public space, so the
	// session reads public only while its content goes to
	// public_root_sessions (D-22). "" means no space: public only.
	ReadAs string
}

// Spaces lists every space p names, for callers that may see only some.
func (p Placement) Spaces() []string {
	out := []string{p.Space}
	if p.ReadAs != "" {
		out = append(out, p.ReadAs)
	}
	for _, r := range p.Roots {
		out = append(out, r.Space)
	}
	if p.Remote != nil && p.Remote.Space != "" {
		out = append(out, p.Remote.Space)
	}
	return out
}

// candidate is one rule's choice of space.
type candidate struct {
	space string
	kind  Kind
}

// Classify decides where content from o goes under res. It reads nothing
// but its arguments.
func Classify(o Origin, res *Resolved, opt Options) Placement {
	if !res.Configured() {
		p := Placement{Space: Shared, Outcome: Placed, Rule: PlaceNoPolicy, Reason: "no spaces policy is applied, so everything is shared"}
		if o.System == SystemSession {
			p.ReadAs = Shared
		}
		return p
	}
	p := classify(o, res, opt)
	if opt.Override != "" {
		over := opt.Override
		if _, ok := res.Space(over); ok || over == Quarantine {
			p.Space, p.Rule, p.Reason = over, PlaceOverride, "the source was retagged to "+over
		} else {
			p.Space, p.Rule, p.Reason = Quarantine, PlaceOverrideUnknown, "the source was retagged to a space the policy no longer has"
		}
		p.Outcome = Placed
		if p.Space == Quarantine {
			p.Outcome = Quarantined
		}
	}
	return p
}

func classify(o Origin, res *Resolved, opt Options) Placement {
	switch o.System {
	case SystemFS, SystemGitHub, SystemSession:
		path := o.Path
		if o.System == SystemGitHub {
			path = ""
		}
		p := placeCheckout(path, o.Remote, res, opt)
		switch o.System {
		case SystemSession:
			sessionPlacement(&p, res)
		case SystemFS:
			publicProof(&p, o, res)
		}
		return p
	case SystemWeb:
		return placeWeb(o, res)
	case SystemClient:
		var p Placement
		if o.WriterSpace == "" {
			return quarantine(p, PlaceUnmatched, "a client note with no pinned session cannot be placed")
		}
		w, ok := writer(o.WriterSpace, res)
		if !ok {
			return quarantine(p, PlaceWriterUnknown, "the writing session's space is not in the policy")
		}
		return placed(p, w.space, PlaceWriter, "written by a session pinned to "+w.space)
	case SystemSlack, SystemTeams:
		return placeChat(o, res)
	}
	return quarantine(Placement{}, PlaceUnmatched, fmt.Sprintf("unknown origin system %q", o.System))
}

// placeCheckout combines the deepest root holding path with the space of
// remote's owner: the stricter wins, and two different spaces of equal
// strictness are a conflict.
func placeCheckout(path string, remote *Repo, res *Resolved, opt Options) Placement {
	var p Placement
	var rootC *candidate
	if p.Roots = matchRoots(path, res); len(p.Roots) > 0 {
		deep := p.Roots[len(p.Roots)-1]
		rootC = &candidate{deep.Space, deep.Kind}
	}
	var remoteC *candidate
	vetoPublic := false
	if remote != nil {
		eff := &RemoteEffect{Repo: *remote}
		p.Remote = eff
		// Reason: public takes only content verified public, so a repo
		// GitHub has not confirmed public never lands there, whatever its
		// owner, even when GitHub cannot be reached.
		vetoPublic = remote.Visibility != VisibilityPublic
		if space, ok := res.GitHubOrgSpace(remote.Owner); ok {
			s, _ := res.Space(space)
			eff.Space, eff.Kind = space, s.Kind
			if opt.Confirmed == nil || !opt.Confirmed(remote.Owner) {
				eff.Effect = "its owner's binding to " + space + " is not confirmed, so its content goes to quarantine"
				return quarantine(p, PlaceOrgUnconfirmed, eff.Effect)
			}
			remoteC = &candidate{space, s.Kind}
			if s.Kind == Public && vetoPublic {
				eff.Effect = fmt.Sprintf("owner bound to %s, but the repo is not verified public", space)
			} else {
				eff.Effect = fmt.Sprintf("owner bound to %s (%s)", space, s.Kind)
			}
		} else {
			eff.Effect = "owner bound to no space"
			if vetoPublic {
				eff.Effect += "; the repo is not verified public, so it cannot be public"
			}
		}
	}
	var c *candidate
	var rule PlaceRule
	switch {
	case rootC == nil && remoteC == nil:
		return quarantine(p, PlaceUnmatched, "no root holds it and its remote's owner is bound to no space")
	case remoteC == nil:
		c, rule = rootC, PlaceRoot
	case rootC == nil:
		c, rule = remoteC, PlaceRemote
	case rootC.space == remoteC.space:
		c, rule = rootC, PlaceAgree
	default:
		rs, _ := rootC.kind.strictness()
		ms, _ := remoteC.kind.strictness()
		switch {
		case ms > rs:
			c, rule = remoteC, PlaceRemoteStricter
		case rs > ms:
			c, rule = rootC, PlaceRootStricter
		default:
			p.Space, p.Outcome, p.Rule = Quarantine, Conflict, PlaceConflict
			p.Reason = fmt.Sprintf("its root is in %s and its remote's owner in %s (both %s)", rootC.space, remoteC.space, rootC.kind)
			return p
		}
	}
	if c.kind == Public && vetoPublic {
		return quarantine(p, PlaceNotVerifiedPublic, "it would be public, but its repo is not verified public")
	}
	return placed(p, c.space, rule, describe(rule, c.space))
}

// publicProof decides local content whose deepest root is public (SP-030,
// SP-076, D-65). Reason: a public root such as ~/code holds every folder the
// user, or any job's session, creates, and anything on disk (a .git, a
// remote, a registered source) can be made by a session, so nothing there
// proves content public. Local content reaches public only when one of the
// public space's public_paths, which only the operator applies, matches it;
// all other local content under a public root is quarantined (or reaches
// public by an operator retag). Even a matched folder stays out when its
// repo is not verified public, or when its remote names a stricter space's
// org: the remote comes from a .git a session can write, so it would let one
// job place content in another's space. A stricter root nested inside the
// public one still decides first.
func publicProof(p *Placement, o Origin, res *Resolved) {
	if len(p.Roots) == 0 {
		return
	}
	deep := p.Roots[len(p.Roots)-1]
	if deep.Kind != Public {
		return
	}
	if p.Outcome != Placed && p.Rule != PlaceNotVerifiedPublic {
		return
	}
	pattern, matched := res.PublicPathFor(deep.Space, o.Path, o.IsDir)
	p.PublicPath = pattern
	switch {
	case matched && p.Outcome == Placed && p.Space == deep.Space:
		p.Rule = PlacePublicPath
		p.Reason = "a public_paths pattern of public space " + deep.Space + " matches it"
	case matched && p.Rule == PlaceNotVerifiedPublic:
		p.Reason = "a public_paths pattern of public space " + deep.Space + " matches it, but its repo is not verified public"
	case matched:
		*p = quarantine(*p, PlaceNoPublicProof,
			"a public_paths pattern of public space "+deep.Space+" matches it, but its remote names another space, and nothing on disk places content; the operator can retag it")
	default:
		*p = quarantine(*p, PlaceNoPublicProof,
			"no public_paths pattern of public space "+deep.Space+" matches it, and local content under a public root is public only through public_paths or an operator retag")
	}
}

// sessionPlacement turns a checkout's placement into a session pin's: a
// session never writes to a public space.
func sessionPlacement(p *Placement, res *Resolved) {
	if p.Outcome != Placed {
		return
	}
	p.ReadAs = p.Space
	if s, _ := res.Space(p.Space); s.Kind == Public {
		p.Space, p.Rule = res.PublicRootSessions, PlacePublicRootSession
		p.Reason = fmt.Sprintf("a session under public space %s reads public only; its content goes to %s (public_root_sessions)", p.ReadAs, p.Space)
		if p.Space == Quarantine {
			p.Outcome = Quarantined
		}
	}
}

// matchRoots returns every root holding path, shallowest first.
func matchRoots(path string, res *Resolved) []RootMatch {
	if path == "" || !filepath.IsAbs(path) {
		return nil
	}
	key := strings.ToLower(filepath.Clean(path))
	var hits []root
	for _, r := range res.roots {
		if key == r.key || strings.HasPrefix(key, strings.TrimSuffix(r.key, string(filepath.Separator))+string(filepath.Separator)) {
			hits = append(hits, r)
		}
	}
	sort.Slice(hits, func(i, j int) bool { return len(hits[i].key) < len(hits[j].key) })
	out := make([]RootMatch, len(hits))
	for i, r := range hits {
		out[i] = RootMatch{Root: r.path, Space: r.space, Kind: r.kind}
	}
	return out
}

// placeWeb places a page by the longest bound prefix it falls under and by
// the session that ingested it (D-14): the stricter wins.
func placeWeb(o Origin, res *Resolved) Placement {
	var p Placement
	var prefixC *candidate
	key, ok := prefixKey(o.URL)
	if !ok {
		// Reason: a URL that cannot be normalized cannot be checked against
		// stricter prefixes, so the writer's space is not trusted for it.
		return quarantine(p, PlaceUnmatched, "the page URL cannot be normalized, so its prefix cannot be checked")
	}
	{
		best := ""
		for _, s := range res.Policy.Spaces {
			for _, prefix := range s.WebPrefixes {
				pk, ok := prefixKey(prefix)
				if ok && len(pk) > len(best) && prefixMatches(key, pk) {
					best, prefixC = pk, &candidate{s.Name, s.Kind}
				}
			}
		}
	}
	if o.WriterSpace == "" {
		if prefixC == nil {
			return quarantine(p, PlaceUnmatched, "the page is under no bound web prefix and no session ingested it")
		}
		return placed(p, prefixC.space, PlaceWebPrefix, "the page is under a web prefix bound to "+prefixC.space)
	}
	w, ok := writer(o.WriterSpace, res)
	if !ok {
		return quarantine(p, PlaceWriterUnknown, "the writing session's space is not in the policy")
	}
	if prefixC == nil || prefixC.space == w.space {
		return placed(p, w.space, PlaceWriter, "ingested by a session pinned to "+w.space)
	}
	ws, _ := w.kind.strictness()
	ps, _ := prefixC.kind.strictness()
	switch {
	case ws > ps:
		return placed(p, w.space, PlaceWriterStricter, fmt.Sprintf("ingested by a session in %s, stricter than the prefix's %s", w.space, prefixC.space))
	case ps > ws:
		return placed(p, prefixC.space, PlacePrefixStricter, fmt.Sprintf("under a prefix bound to %s, stricter than the session's %s", prefixC.space, w.space))
	}
	p.Space, p.Outcome, p.Rule = Quarantine, Conflict, PlaceConflict
	p.Reason = fmt.Sprintf("its prefix is bound to %s and the session is in %s", prefixC.space, w.space)
	return p
}

// prefixMatches reports whether url key k falls under prefix key pk, at a
// path boundary, so https://a.com does not match https://a.com.evil. Keys
// carry no trailing slash.
func prefixMatches(k, pk string) bool {
	return k == pk || strings.HasPrefix(k, pk+"/")
}

// writer is the space a pinned session writes to. Sessions never write to
// public, so a pin naming a public space is not trusted.
func writer(space string, res *Resolved) (candidate, bool) {
	s, ok := res.Space(space)
	if !ok || s.Kind == Public {
		return candidate{}, false
	}
	return candidate{s.Name, s.Kind}, true
}

func placeChat(o Origin, res *Resolved) Placement {
	key := strings.ToLower(string(o.System) + ":" + o.Tenant)
	if o.Tenant != "" {
		for _, s := range res.Policy.Spaces {
			for _, t := range s.Chat {
				if strings.ToLower(t) == key {
					return placed(Placement{}, s.Name, PlaceChatTenant, fmt.Sprintf("%s tenant bound to %s", o.System, s.Name))
				}
			}
		}
	}
	return quarantine(Placement{}, PlaceUnmatched, fmt.Sprintf("the %s tenant is bound to no space", o.System))
}

func placed(p Placement, space string, rule PlaceRule, reason string) Placement {
	p.Space, p.Outcome, p.Rule, p.Reason = space, Placed, rule, reason
	return p
}

func quarantine(p Placement, rule PlaceRule, reason string) Placement {
	p.Space, p.Outcome, p.Rule, p.Reason = Quarantine, Quarantined, rule, reason
	return p
}

func describe(rule PlaceRule, space string) string {
	switch rule {
	case PlaceRoot:
		return "the deepest root holding it is bound to " + space
	case PlaceRemote:
		return "no root holds it; its remote's owner is bound to " + space
	case PlaceAgree:
		return "its root and its remote's owner are both bound to " + space
	case PlaceRemoteStricter:
		return "its remote's owner is bound to " + space + ", stricter than its root"
	case PlaceRootStricter:
		return "its root is bound to " + space + ", stricter than its remote's owner"
	}
	return string(rule)
}
