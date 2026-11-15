package daemon

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"
	"unicode"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/rag"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// quarantineHolder is what the quarantine RPCs need from the RAG store
// (implemented by *rag.Service).
type quarantineHolder interface {
	ListQuarantine(ctx context.Context) ([]rag.QuarantinedSource, int, error)
	PurgeQuarantine(ctx context.Context, uris []string, all, apply bool) (rag.PurgeResult, error)
}

// maxQuarantineListed bounds the sources one ListQuarantine answer names, so
// a large quarantine never exceeds gRPC's message size; the totals still
// count every source.
const maxQuarantineListed = 5000

// quarantine returns the store's quarantine, or a FailedPrecondition error
// when the store has none.
func (s *server) quarantine() (quarantineHolder, error) {
	q, ok := s.rag.(quarantineHolder)
	if !ok || q == nil {
		return nil, grpcstatus.Error(codes.FailedPrecondition, "the RAG store is unavailable")
	}
	return q, nil
}

// ListQuarantine is the RPC: operator only, since quarantine is readable by
// no session (INV-03) and the list names its sources.
func (s *server) ListQuarantine(ctx context.Context, _ *orchdv1.ListQuarantineRequest) (*orchdv1.ListQuarantineResponse, error) {
	if err := refuseNonOperator(ctx, orchdv1.Orchd_ListQuarantine_FullMethodName, "list quarantine"); err != nil {
		return nil, err
	}
	q, err := s.quarantine()
	if err != nil {
		return nil, err
	}
	held, chunks, err := q.ListQuarantine(ctx)
	if err != nil {
		log.Printf("orchd: listing quarantine failed: %v", err)
		return nil, grpcstatus.Error(codes.Unavailable, "could not read quarantine; see orchd's log")
	}
	resp := &orchdv1.ListQuarantineResponse{TotalSources: int32(len(held)), TotalChunks: int32(chunks)}
	for i, h := range held {
		if i == maxQuarantineListed {
			break
		}
		resp.Sources = append(resp.Sources, &orchdv1.QuarantineSource{SourceUri: h.SourceURI,
			SourceType: h.SourceType, Chunks: int32(h.Chunks), IngestedAt: h.IngestedAt})
	}
	return resp, nil
}

// PurgeQuarantine is the RPC: operator only. It counts unless confirm is set;
// the gate is here, not only in the CLI, so every caller opts in
// deliberately. A named source quarantine does not hold refuses the purge
// and is never deleted from anywhere else.
func (s *server) PurgeQuarantine(ctx context.Context, req *orchdv1.PurgeQuarantineRequest) (*orchdv1.PurgeQuarantineResponse, error) {
	if err := refuseNonOperator(ctx, orchdv1.Orchd_PurgeQuarantine_FullMethodName, "purge quarantine"); err != nil {
		return nil, err
	}
	uris := req.GetSourceUris()
	if req.GetAll() == (len(uris) > 0) {
		return nil, grpcstatus.Error(codes.InvalidArgument, "name the sources to purge, or set all; not both and not neither")
	}
	for _, u := range uris {
		if u == "" {
			return nil, grpcstatus.Error(codes.InvalidArgument, "a source_uri is empty")
		}
	}
	q, err := s.quarantine()
	if err != nil {
		return nil, err
	}
	res, err := q.PurgeQuarantine(ctx, uris, req.GetAll(), req.GetConfirm())
	switch {
	case errors.Is(err, rag.ErrNotQuarantined):
		// Reason: the answer is the same whether the source is elsewhere or
		// nowhere, so a purge never reveals where a source lives.
		return nil, grpcstatus.Error(codes.FailedPrecondition, termSafeErr(err))
	case err != nil:
		log.Printf("orchd: quarantine purge by %s stopped after %d chunk(s): %v", callerLabel(ctx), res.Chunks, err)
		s.quarantineCounts.forget()
		return nil, grpcstatus.Errorf(codes.Unavailable, "purge stopped after %d chunk(s); run it again to finish (see orchd's log)", res.Chunks)
	}
	if res.Purged {
		log.Printf("orchd: quarantine purge by %s deleted %d source(s), %d chunk(s)", callerLabel(ctx), res.Sources, res.Chunks)
		s.quarantineCounts.forget()
	}
	return &orchdv1.PurgeQuarantineResponse{Sources: int32(res.Sources), Chunks: int32(res.Chunks), Purged: res.Purged}, nil
}

// termSafeErr is err's text with control and format characters (a bidi
// override among them) replaced: a refused URI is echoed back to the
// caller's terminal.
func termSafeErr(err error) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '?'
		}
		return r
	}, err.Error())
}

// quarantineCountTTL bounds how often Status scans quarantine: the app
// polls Status, and counting reads the whole collection.
const quarantineCountTTL = 30 * time.Second

// quarantineCountTimeout keeps a slow store from delaying Status.
const quarantineCountTimeout = 2 * time.Second

// quarantineCountCache holds the last counts and when they were taken.
type quarantineCountCache struct {
	mu     sync.Mutex
	counts *orchdv1.QuarantineCounts
	// at is when counts were taken, or when counting last failed.
	at time.Time
	// failed says the last count failed, so Status does not scan again
	// until the TTL ends while the store is slow or down.
	failed bool
}

// forget drops the cached counts, after a purge changed them.
func (c *quarantineCountCache) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts, c.failed = nil, false
}

// quarantineStatus is what quarantine holds, for Status: counts only, never
// a source, since Status is open to every caller. Nil when the store cannot
// be counted.
func (s *server) quarantineStatus(ctx context.Context) *orchdv1.QuarantineCounts {
	q, ok := s.rag.(quarantineHolder)
	if !ok || q == nil {
		return nil
	}
	c := &s.quarantineCounts
	c.mu.Lock()
	defer c.mu.Unlock()
	if (c.counts != nil || c.failed) && time.Since(c.at) < quarantineCountTTL {
		return c.counts
	}
	ctx, cancel := context.WithTimeout(ctx, quarantineCountTimeout)
	defer cancel()
	held, chunks, err := q.ListQuarantine(ctx)
	c.at, c.failed, c.counts = time.Now(), err != nil, nil
	if err == nil {
		c.counts = &orchdv1.QuarantineCounts{Sources: int32(len(held)), Chunks: int32(chunks)}
	}
	return c.counts
}
