package rag

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Origin systems: the kind of writer content comes from (SP-023). They
// match spaces.System, which the placer maps them to.
const (
	OriginFS      = "fs"
	OriginGitHub  = "github"
	OriginWeb     = "web"
	OriginSession = "session"
	OriginClient  = "client"
)

// Origin is the facts a writer reports about content it stores: where it
// came from, never where it goes. orchd's placer reads them to decide the
// chunk's space (spaces.Classify), and every chunk keeps them under the
// payload field "origin", so a later relabel can place it again without the
// writer. Only orchd's own code fills it; no RPC field reaches it.
type Origin struct {
	// System is one of the Origin* constants.
	System string
	// Path is a local file or folder: a checkout's file or a hand-off. For
	// a session record it is the session's first cwd, when known.
	Path string
	// URL is a web page's address.
	URL string
	// RepoOwner and RepoName are the GitHub repo content belongs to.
	RepoOwner, RepoName string
	// Visibility is the repo's visibility as GitHub reported it: "public",
	// "private" or "" when unknown, which is never treated as public.
	Visibility string
	// OwnerNodeID is the repo owner's GitHub node ID, as GitHub reported it
	// with the repo.
	OwnerNodeID string
	// Session is the session a session record or transcript belongs to.
	Session string
	// Caller is who stored a client write: "session", "task", "operator"
	// or "none" (an unidentified caller).
	Caller string
	// WriterSpace is the space of the pinned session that stored a client
	// write, as orchd resolved it from the caller's process; "" for none.
	WriterSpace string
	// Tenant, Container and ContainerKind say where chat content was
	// written (SP-060): the tenant's stable ID without its provider, the
	// channel, chat or meeting's stable ID, and one of ChatKinds.
	Tenant, Container, ContainerKind string
	// Participants are the stable IDs of a chat container's members, as the
	// feed reported them, sorted and joined by commas (an ID holds none).
	// A string keeps Origin comparable.
	Participants string
}

// Payload fields of placement.
const (
	fieldOrigin  = "origin"
	fieldSpace   = "space"
	fieldLabelsV = "labels_v"
)

// payload is o as stored: only the facts the writer reported.
func (o Origin) payload() map[string]any {
	out := map[string]any{}
	for k, v := range map[string]string{
		"system": o.System, "path": o.Path, "url": o.URL, "repo_owner": o.RepoOwner, "repo_name": o.RepoName,
		"visibility": o.Visibility, "owner_node_id": o.OwnerNodeID, "session": o.Session, "caller": o.Caller,
		"writer_space": o.WriterSpace, "tenant": o.Tenant, "container": o.Container, "container_kind": o.ContainerKind,
		"participants": o.Participants,
	} {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// key is o in a stable form, for the write's hash.
func (o Origin) key() string {
	key := strings.Join([]string{o.System, o.Path, o.URL, o.RepoOwner, o.RepoName, o.Visibility, o.OwnerNodeID,
		o.Session, o.Caller, o.WriterSpace}, "\x01")
	// Reason: an origin without chat facts keeps the key, and so the
	// ingest hash, it had before chat origins existed.
	if o.Tenant != "" || o.Container != "" || o.ContainerKind != "" || o.Participants != "" {
		key += "\x01" + strings.Join([]string{o.Tenant, o.Container, o.ContainerKind, o.Participants}, "\x01")
	}
	return key
}

// originFrom reads a stored "origin" payload back.
func originFrom(v any) Origin {
	m, _ := v.(map[string]any)
	return Origin{
		System: str(m["system"]), Path: str(m["path"]), URL: str(m["url"]),
		RepoOwner: str(m["repo_owner"]), RepoName: str(m["repo_name"]), Visibility: str(m["visibility"]),
		OwnerNodeID: str(m["owner_node_id"]), Session: str(m["session"]), Caller: str(m["caller"]),
		WriterSpace: str(m["writer_space"]), Tenant: str(m["tenant"]), Container: str(m["container"]),
		ContainerKind: str(m["container_kind"]), Participants: str(m["participants"]),
	}
}

// labelsV reads a stored labels_v: JSON numbers decode as float64.
func labelsV(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}

// PlaceFunc decides a write's space from m.Origin and sets m.Space and
// m.LabelsV. An error stops the write: nothing is stored.
type PlaceFunc func(ctx context.Context, m *Meta) error

// place runs the service's placer on m. Whatever m carried in Space and
// LabelsV is cleared first, so only the placer's answer is stored.
func (s *Service) place(ctx context.Context, m *Meta) error {
	if s.Place == nil {
		return nil
	}
	m.Space, m.LabelsV = "", 0
	if m.Origin.System == "" {
		// Reason: a write that reports no origin cannot be placed, and an
		// unplaced chunk must not be stored where a reader could take it
		// for placed content.
		return fmt.Errorf("%s reports no origin, so it cannot be placed", m.SourceURI)
	}
	if err := s.Place(ctx, m); err != nil {
		return err
	}
	if m.Space == "" {
		return fmt.Errorf("%s was not placed in any space", m.SourceURI)
	}
	return nil
}
