package spaces

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"time"
)

// OrgIdentity is a GitHub account's login, as GitHub spells it, and its node
// ID. A deleted login can be registered again by someone else; the node ID
// cannot, so it is what a binding trusts.
type OrgIdentity struct {
	Login  string
	NodeID string
}

var (
	// ErrOrgLookup is returned when a bound GitHub org's identity could not be
	// read from GitHub. Apply refuses rather than bind an org unchecked.
	ErrOrgLookup = errors.New("could not confirm a GitHub org's identity")
	// ErrOrgChanged is returned by Apply when a bound org's node ID differs
	// from the one confirmed earlier and the operator did not confirm it.
	ErrOrgChanged = errors.New("a GitHub org's identity changed since it was bound")
)

// orgKey is how bindings match logins: GitHub logins are case-insensitive.
func orgKey(login string) string { return strings.ToLower(strings.TrimSpace(login)) }

// boundOrgs returns every GitHub org p binds, keyed by orgKey, with its
// login as written in the policy.
func boundOrgs(p Policy) map[string]string {
	orgs := map[string]string{}
	for _, s := range p.Spaces {
		for _, org := range s.GitHubOrgs {
			if k := orgKey(org); k != "" {
				orgs[k] = strings.TrimSpace(org)
			}
		}
	}
	return orgs
}

// lookupOrg reads login's identity from GitHub through env.LookupOrg, as the
// gh profile of the space res binds it to.
func (e Env) lookupOrg(ctx context.Context, res *Resolved, login string) (OrgIdentity, error) {
	if e.LookupOrg == nil {
		return OrgIdentity{}, fmt.Errorf("%w %q: no GitHub lookup is configured", ErrOrgLookup, login)
	}
	prof, _ := res.GHProfileFor(login)
	id, err := e.LookupOrg(ctx, login, prof)
	switch {
	case err != nil:
		return OrgIdentity{}, fmt.Errorf("%w %q: %v", ErrOrgLookup, login, err)
	case orgKey(id.Login) != orgKey(login) || id.NodeID == "":
		return OrgIdentity{}, fmt.Errorf("%w %q: GitHub answered for another account", ErrOrgLookup, login)
	}
	return id, nil
}

// resolveOrgs looks up every org revision n binds, before Apply takes the
// write lock, so slow GitHub calls never hold up other writers. When Apply
// will refuse n anyway (not the newest, an unconfirmed agent draft, or not
// parseable) it resolves nothing and leaves Apply to say why.
func (e *Engine) resolveOrgs(ctx context.Context, n int64, acceptAgentDraft bool) (map[string]OrgIdentity, error) {
	r, err := Latest(ctx, e.db)
	if err != nil || r == nil || r.Number != n || !r.ByOperator && !acceptAgentDraft {
		return nil, nil
	}
	p, err := Parse(r.Doc)
	if err != nil {
		return nil, nil
	}
	res, err := Validate(p, e.env)
	if err != nil {
		return nil, nil
	}
	ids := map[string]OrgIdentity{}
	for k, login := range boundOrgs(p) {
		id, err := e.env.lookupOrg(ctx, res, login)
		if err != nil {
			return nil, err
		}
		ids[k] = id
	}
	return ids, nil
}

// orgBinding is one row of spaces_org_bindings.
type orgBinding struct {
	login     string
	nodeID    string
	suspended bool
}

func loadBinding(ctx context.Context, q querier, key string) (*orgBinding, error) {
	var b orgBinding
	var suspended sql.NullString
	err := q.QueryRowContext(ctx, `SELECT login, node_id, suspended_at FROM spaces_org_bindings WHERE org = ?`, key).
		Scan(&b.login, &b.nodeID, &suspended)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read GitHub org binding: %w", err)
	}
	b.suspended = suspended.Valid
	return &b, nil
}

// bindOrgs records the bindings of res, inside Apply's
// transaction. An org whose node ID differs from its earlier binding is
// refused unless confirm names it, so an apply made for another reason never
// accepts a changed org by accident. Re-binding clears any suspension.
func bindOrgs(ctx context.Context, tx *sql.Tx, res *Resolved, revision int64, ids map[string]OrgIdentity, confirm []string, now time.Time) error {
	orgs := boundOrgs(res.Policy)
	keys := make([]string, 0, len(orgs))
	for k := range orgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	confirmed := func(k string) bool {
		return slices.ContainsFunc(confirm, func(c string) bool { return orgKey(c) == k })
	}
	for _, k := range keys {
		id, ok := ids[k]
		if !ok {
			return fmt.Errorf("%w (GitHub org %q was not looked up)", ErrStale, orgs[k])
		}
		prev, err := loadBinding(ctx, tx, k)
		if err != nil {
			return err
		}
		if prev != nil && prev.nodeID != id.NodeID && !confirmed(k) {
			return fmt.Errorf("%w: %q is now node %s, was %s", ErrOrgChanged, id.Login, id.NodeID, prev.nodeID)
		}
	}
	// Reason: rows of orgs a revision drops are kept, so dropping an org and
	// adding it back later cannot bind a different account unconfirmed.
	at := now.UTC().Format(time.RFC3339)
	for _, k := range keys {
		id := ids[k]
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO spaces_org_bindings (org, login, node_id, revision, bound_at) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (org) DO UPDATE SET login = excluded.login, node_id = excluded.node_id, revision = excluded.revision,
			   bound_at = excluded.bound_at, suspended_at = NULL, seen_node_id = NULL`,
			k, id.Login, id.NodeID, revision, at); err != nil {
			return fmt.Errorf("bind GitHub orgs: %w", err)
		}
	}
	return nil
}

// OrgSpace returns the space new content from GitHub account login goes to:
// "" when the applied policy binds it to no space, Quarantine while its
// binding is suspended or was never confirmed, else its space. It reads only
// orch.db; CheckOrg also asks GitHub.
func (e *Engine) OrgSpace(ctx context.Context, login string) (string, error) {
	res, err := e.Current()
	if err != nil {
		return "", err
	}
	space, b, err := e.orgBinding(ctx, res, login)
	switch {
	case err != nil, space == "":
		return "", err
	case b == nil || b.suspended:
		return Quarantine, nil
	}
	return space, nil
}

// orgBinding returns the space login is bound to and its stored binding,
// under res.
func (e *Engine) orgBinding(ctx context.Context, res *Resolved, login string) (string, *orgBinding, error) {
	space, ok := res.GitHubOrgSpace(login)
	if !ok {
		return "", nil, nil
	}
	b, err := loadBinding(ctx, e.db, orgKey(login))
	return space, b, err
}

// CheckOrg re-checks a bound org's node ID, for each sync of its content,
// and returns where new content goes as OrgSpace does. A node ID other than
// the one confirmed at apply suspends the binding: the org's new content goes
// to quarantine and orch status reports it until the operator confirms the
// new identity at an apply. A lookup failure is an error; the caller must not
// place the content.
func (e *Engine) CheckOrg(ctx context.Context, login string) (string, error) {
	res, err := e.Current()
	if err != nil {
		return "", err
	}
	space, b, err := e.orgBinding(ctx, res, login)
	switch {
	case err != nil, space == "":
		return "", err
	case b == nil || b.suspended:
		return Quarantine, nil
	}
	id, err := e.env.lookupOrg(ctx, res, login)
	if err != nil {
		return "", err
	}
	if id.NodeID == b.nodeID {
		return space, nil
	}
	// Reason: the node_id condition keeps a check that raced an apply from
	// suspending the binding that apply just confirmed.
	if _, err := e.db.ExecContext(ctx,
		`UPDATE spaces_org_bindings SET suspended_at = ?, seen_node_id = ? WHERE org = ? AND node_id = ? AND suspended_at IS NULL`,
		e.now().UTC().Format(time.RFC3339), id.NodeID, orgKey(login), b.nodeID); err != nil {
		return "", fmt.Errorf("suspend GitHub org binding: %w", err)
	}
	// Reason: tasks can read orchd's log, so it names no org; the operator
	// sees which in 'orch spaces diff'.
	log.Printf("orchd: spaces: a bound GitHub org's identity changed; its new content goes to quarantine until the operator confirms")
	return Quarantine, nil
}

// UnconfirmedOrgs lists the applied policy's GitHub orgs whose new content
// goes to quarantine: suspended bindings, and orgs with no binding (applied
// before bindings were recorded). It names orgs, so only the operator may see
// it.
func (e *Engine) UnconfirmedOrgs(ctx context.Context) ([]string, error) {
	res, err := e.Current()
	if err != nil || !res.Configured() {
		return nil, nil
	}
	orgs := boundOrgs(res.Policy)
	var out []string
	for k, login := range orgs {
		b, err := loadBinding(ctx, e.db, k)
		if err != nil {
			return nil, err
		}
		if b == nil || b.suspended {
			out = append(out, login)
		}
	}
	sort.Slice(out, func(i, j int) bool { return orgKey(out[i]) < orgKey(out[j]) })
	return out, nil
}

// Place classifies o under the enforced policy, confirming its remote's
// GitHub org binding from orch.db. ErrBroken means nothing can be placed.
func (e *Engine) Place(ctx context.Context, o Origin) (Placement, error) {
	res, err := e.Current()
	if err != nil {
		return Placement{}, err
	}
	return e.PlaceUnder(ctx, res, o)
}

// PlaceUnder is Place under res, for a caller that checks the answer against
// the same policy it was placed under.
func (e *Engine) PlaceUnder(ctx context.Context, res *Resolved, o Origin) (Placement, error) {
	var opt Options
	if o.Remote != nil && res.Configured() {
		if _, ok := res.GitHubOrgSpace(o.Remote.Owner); ok {
			b, err := loadBinding(ctx, e.db, orgKey(o.Remote.Owner))
			if err != nil {
				return Placement{}, err
			}
			held := b != nil && !b.suspended
			opt.Confirmed = func(string) bool { return held }
		}
	}
	return Classify(o, res, opt), nil
}
