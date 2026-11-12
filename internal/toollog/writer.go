package toollog

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultQueue is how many events may wait to be stored. Past it, events are
// dropped and counted, and a gap record says so (D-30, RC-29), so a flood
// cannot build up memory in orchd.
const DefaultQueue = 4096

// MaxQueuedBytes bounds the size of the events waiting to be stored, as
// their callers reported them, so a flood of large events cannot hold much
// memory either (H1 of the SP-014 audit).
const MaxQueuedBytes = 64 << 20

// MaxReservedBytes bounds the room kept for guard decisions (D-37), as
// MaxQueuedBytes bounds the ordinary queue.
const MaxReservedBytes = 16 << 20

// reserveShare is the part of the queue's length kept apart for guard
// decisions: EnqueueDecision uses it once an ordinary share is full.
const reserveShare = 4

// ownerShare is the part of the queue, in events and in bytes, that one
// quota key (a harness process or a task) may hold: a session flooding the
// log drops its own events, not other sessions', however many sessions it
// pins.
const ownerShare = 4

// maxDropKeys bounds the drop counters. Past it, a drop whose claimed
// session has no counter yet is counted under the caller's own chain, so a
// hook naming a new session for every dropped event cannot grow memory.
const maxDropKeys = 4096

// maxBatch bounds how many queued jobs one transaction stores.
const maxBatch = 256

// gapFlush is how often counted drops of owners with no later event are
// written as gap records.
const gapFlush = time.Second

// ErrClosed is returned once the writer has stopped.
var ErrClosed = errors.New("command log is closed")

// Key says whom an event is charged to and which chain it goes to.
type Key struct {
	// Quota is who holds the event's share of the queue: one key per
	// harness process or task, so minting sessions gains no room.
	Quota string
	// Owner is the chain of the caller orchd resolved. Claim, when set, is
	// a session the hook named instead; the writer's resolver decides
	// whether the event goes to it.
	Owner Owner
	Claim string
}

// dropKey counts drops by where the dropped events would have gone.
type dropKey struct {
	owner Owner
	claim string
}

// Resolver turns a key into the chain its events go to: Owner, or the
// claimed session when the caller may write to it. It runs on the writer
// goroutine and may block briefly (it may read orch.db). It must return a
// chain of the same kind as Owner; anything else is ignored.
type Resolver func(Key) Owner

// job is one unit of work for the writer goroutine.
type job struct {
	key Key
	// build makes an event record; it runs on the writer goroutine, so
	// slow work such as reading a call's targets stays off the hook's path.
	build func() Record
	// end writes the owner's end record and anchors it.
	end bool
	at  time.Time
	// size is the event's size as its caller reported it, for the queue's
	// byte budget.
	size int64
	// gap is how many events of the job's key were dropped since its last
	// queued job; a gap record says so just before this one.
	gap int64
	// done, when set, receives the result once the job's batch committed.
	done chan error
	// reserved marks an event queued in the room kept for guard decisions.
	reserved bool
}

type share struct {
	n     int
	bytes int64
}

type chainHead struct {
	seq  int64
	hash string
	kind string
}

// Writer appends records to a Store from one goroutine, which keeps each
// owner's records in the order they were queued.
type Writer struct {
	store *Store
	jobs  chan job
	now   func() time.Time

	resolve Resolver
	// onAnchor, when set, is given the anchors each committed batch wrote.
	onAnchor func([]Anchor)

	mu    sync.Mutex
	drops map[dropKey]int64
	// queued is what each quota key holds of the queue, and queuedBytes the
	// total; an event's share is released once its record is built.
	queued      map[string]*share
	queuedBytes int64
	// Ordinary events fill the channel up to size; the reserve slots past
	// it are kept for guard decisions (D-37), with reserved, reservedN and
	// reservedBytes counting them as queued and queuedBytes do.
	size, reserve int
	reserved      map[string]*share
	reservedN     int
	reservedBytes int64
	// closed is set under mu before jobs is closed, so no send races it.
	closed bool

	dropped atomic.Int64
	stored  atomic.Int64
	// heads caches each owner's last record; only the writer goroutine
	// uses it.
	heads map[Owner]chainHead
	stop  chan struct{}
	// exited closes when the writer goroutine has returned.
	exited chan struct{}
}

// NewWriter starts a writer over s with a queue of queue events. resolve
// may be nil: every event then goes to its key's Owner.
func NewWriter(s *Store, queue int, resolve Resolver) *Writer {
	return NewWriterWithAnchors(s, queue, resolve, nil)
}

// NewWriterWithAnchors is NewWriter that also hands onAnchor the anchors of
// each batch once it is committed (D-35: orchd copies them into orch.db).
// onAnchor runs on the writer goroutine; it should be quick.
func NewWriterWithAnchors(s *Store, queue int, resolve Resolver, onAnchor func([]Anchor)) *Writer {
	reserve := max(queue/reserveShare, 1)
	w := &Writer{
		store: s, jobs: make(chan job, queue+reserve), now: time.Now, resolve: resolve, onAnchor: onAnchor,
		drops: map[dropKey]int64{}, queued: map[string]*share{}, heads: map[Owner]chainHead{},
		stop: make(chan struct{}), exited: make(chan struct{}),
		size: queue, reserve: reserve, reserved: map[string]*share{},
	}
	go w.run()
	return w
}

// Enqueue queues an event record for k without blocking; size is the
// event's size in bytes. It returns false, and counts the event toward the
// next gap record of k's chain, when the writer is closed, the queue or its
// byte budget is full, or k's quota key already holds its share of either.
func (w *Writer) Enqueue(k Key, size int64, build func() Record) bool {
	return w.enqueue(k, size, build, false)
}

// EnqueueDecision is Enqueue for a guard decision, such as a refused call
// (D-37): once k's ordinary share or the ordinary queue is full, it is
// queued in room kept for guard decisions, so a session flooding the log
// with ordinary events cannot push its refusals out. That room has shares
// of its own, so one quota key cannot take all of it either.
func (w *Writer) EnqueueDecision(k Key, size int64, build func() Record) bool {
	return w.enqueue(k, size, build, true)
}

func (w *Writer) enqueue(k Key, size int64, build func() Record, decision bool) bool {
	if k.Claim == k.Owner.ID {
		k.Claim = ""
	}
	dk := dropKey{k.Owner, k.Claim}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		w.countDrop(dk, k)
		return false
	}
	j := job{key: k, build: build, at: w.now(), size: size, gap: w.drops[dk]}
	sh := w.queued[k.Quota]
	if sh == nil {
		sh = &share{}
	}
	// Reason: ordinary events may fill the channel up to size; the room
	// past it is for guard decisions.
	if len(w.jobs) < w.size && w.queuedBytes+size <= MaxQueuedBytes &&
		sh.n < max(w.size/ownerShare, 1) && sh.bytes+size <= MaxQueuedBytes/ownerShare && w.send(j, dk) {
		sh.n++
		sh.bytes += size
		w.queued[k.Quota] = sh
		w.queuedBytes += size
		return true
	}
	if decision {
		rs := w.reserved[k.Quota]
		if rs == nil {
			rs = &share{}
		}
		j.reserved = true
		if w.reservedN < w.reserve && w.reservedBytes+size <= MaxReservedBytes &&
			rs.n < max(w.reserve/ownerShare, 1) && rs.bytes+size <= MaxReservedBytes/ownerShare && w.send(j, dk) {
			rs.n++
			rs.bytes += size
			w.reserved[k.Quota] = rs
			w.reservedN++
			w.reservedBytes += size
			return true
		}
	}
	w.countDrop(dk, k)
	return false
}

// send queues j without blocking and clears dk's drop count, which j's gap
// carries. It is called with mu held.
func (w *Writer) send(j job, dk dropKey) bool {
	select {
	case w.jobs <- j:
		delete(w.drops, dk)
		return true
	default:
		return false
	}
}

// countDrop counts one dropped event of k toward dk's next gap record. It
// is called with mu held.
func (w *Writer) countDrop(dk dropKey, k Key) {
	if _, ok := w.drops[dk]; !ok && len(w.drops) >= maxDropKeys {
		dk = dropKey{owner: k.Owner}
	}
	w.drops[dk]++
	w.dropped.Add(1)
}

// release returns an event's share of the queue.
func (w *Writer) release(j job) {
	w.mu.Lock()
	defer w.mu.Unlock()
	shares := w.queued
	if j.reserved {
		shares = w.reserved
		w.reservedN--
		w.reservedBytes -= j.size
	} else {
		w.queuedBytes -= j.size
	}
	if sh := shares[j.key.Quota]; sh != nil {
		sh.n--
		sh.bytes -= j.size
		if sh.n <= 0 {
			delete(shares, j.key.Quota)
		}
	}
}

// End writes o's end record after everything already queued for it, and
// anchors it as the chain head (SessionEnd). A chain whose last record is
// already an end record is left as it is, so repeated ends add nothing but
// a resumed session's later records are anchored again. It waits for the
// write.
func (w *Writer) End(ctx context.Context, o Owner) error {
	return w.wait(ctx, job{key: Key{Owner: o}, end: true})
}

// Flush waits until everything queued before it is stored.
func (w *Writer) Flush(ctx context.Context) error {
	return w.wait(ctx, job{})
}

func (w *Writer) wait(ctx context.Context, j job) error {
	j.at = w.now()
	j.done = make(chan error, 1)
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return ErrClosed
	}
	// An end record comes after the gap of events dropped before it.
	dk := dropKey{owner: j.key.Owner}
	if j.end {
		j.gap = w.drops[dk]
		delete(w.drops, dk)
	}
	// A control job waits for room rather than being dropped; mu is
	// released first so Enqueue never waits behind it.
	w.mu.Unlock()
	unsent := func() {
		if j.gap > 0 {
			w.mu.Lock()
			w.drops[dk] += j.gap
			w.mu.Unlock()
		}
	}
	select {
	case w.jobs <- j:
	case <-w.stop:
		unsent()
		return ErrClosed
	case <-ctx.Done():
		unsent()
		return ctx.Err()
	}
	select {
	case err := <-j.done:
		return err
	case <-w.exited:
		// The writer may have stored it before exiting.
		select {
		case err := <-j.done:
			return err
		default:
			// Sent after the writer drained: nothing stored it.
			unsent()
			return ErrClosed
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Dropped is how many events were dropped since the writer started.
func (w *Writer) Dropped() int64 { return w.dropped.Load() }

// Stored is how many records were stored since the writer started.
func (w *Writer) Stored() int64 { return w.stored.Load() }

// Close stores what is queued, then stops the writer and waits for it.
func (w *Writer) Close() {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.stop)
	}
	w.mu.Unlock()
	<-w.exited
}

func (w *Writer) run() {
	defer close(w.exited)
	tick := time.NewTicker(gapFlush)
	defer tick.Stop()
	for {
		var batch []job
		select {
		case j := <-w.jobs:
			batch = append(batch, j)
		case <-tick.C:
		case <-w.stop:
			w.drain()
			return
		}
	fill:
		for len(batch) < maxBatch {
			select {
			case j := <-w.jobs:
				batch = append(batch, j)
			default:
				break fill
			}
		}
		w.commit(batch)
	}
}

// drain stores whatever is still queued at Close.
func (w *Writer) drain() {
	for {
		var batch []job
	fill:
		for len(batch) < maxBatch {
			select {
			case j := <-w.jobs:
				batch = append(batch, j)
			default:
				break fill
			}
		}
		if len(batch) == 0 {
			w.commit(nil)
			return
		}
		w.commit(batch)
	}
}

// commit stores one batch and the gap records of counted drops in one
// transaction. A failed batch is counted as dropped, so its loss shows as a
// gap once the store works again.
func (w *Writer) commit(batch []job) {
	built := make([]bool, len(batch))
	defer func() {
		for i, j := range batch {
			if j.build != nil && !built[i] {
				w.release(j)
			}
		}
	}()
	gaps := w.takeDrops()
	// Chains are resolved before the transaction: the resolver may read
	// orch.db.
	chains := make([]Owner, len(batch))
	for i, j := range batch {
		chains[i] = w.chainOf(j.key)
	}
	gapChains := make(map[dropKey]Owner, len(gaps))
	for dk := range gaps {
		gapChains[dk] = w.chainOf(Key{Owner: dk.owner, Claim: dk.claim})
	}
	if len(batch) == 0 && len(gaps) == 0 {
		return
	}
	ctx := context.Background()
	pending := map[Owner]chainHead{}
	var n int64
	var anchors []Anchor
	err := w.store.inTx(ctx, func(tx execer) error {
		appendRec := func(r Record) error {
			o := Owner{Kind: r.OwnerKind, ID: r.OwnerID}
			h, ok := pending[o]
			if !ok {
				if h, ok = w.heads[o]; !ok {
					var err error
					if h, err = head(ctx, tx, o); err != nil {
						return err
					}
				}
			}
			r.Seq, r.PrevHash = h.seq+1, h.hash
			r.Hash = r.computeHash()
			if err := insert(ctx, tx, r); err != nil {
				return err
			}
			pending[o] = chainHead{seq: r.Seq, hash: r.Hash, kind: r.Kind}
			n++
			if r.Kind == KindEnd {
				anchors = append(anchors, Anchor{Owner: o, Seq: r.Seq, Hash: r.Hash, At: r.TS})
				return anchor(ctx, tx, r)
			}
			return nil
		}
		gapRec := func(o Owner, dropped int64, at time.Time) error {
			return appendRec(Record{OwnerKind: o.Kind, OwnerID: o.ID, TS: at.UnixNano(), Kind: KindGap, Dropped: dropped})
		}
		now := w.now()
		for i, j := range batch {
			o := chains[i]
			if o == (Owner{}) {
				continue
			}
			if j.gap > 0 {
				if err := gapRec(o, j.gap, now); err != nil {
					return err
				}
			}
			var r Record
			if j.end {
				last, ok := pending[o]
				if !ok {
					if last, ok = w.heads[o]; !ok {
						var err error
						if last, err = head(ctx, tx, o); err != nil {
							return err
						}
					}
				}
				if last.kind == KindEnd || last.seq == 0 {
					continue
				}
				r = Record{TS: j.at.UnixNano(), Kind: KindEnd}
			} else {
				r = build(j)
				r.Kind = KindEvent
				built[i] = true
				w.release(j)
				// The request is no longer needed; let it be collected
				// while the rest of the batch is built.
				batch[i].build = nil
			}
			r.OwnerKind, r.OwnerID = o.Kind, o.ID
			if err := appendRec(r); err != nil {
				return err
			}
		}
		// Drops no later queued event carries happened after this batch
		// was queued.
		for dk, d := range gaps {
			if err := gapRec(gapChains[dk], d, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		for o, h := range pending {
			w.heads[o] = h
		}
		// An ended chain is rarely written again; a resumed session's
		// next record reads its head from the store.
		for i, j := range batch {
			if j.end {
				delete(w.heads, chains[i])
			}
		}
		w.stored.Add(n)
		if len(anchors) > 0 && w.onAnchor != nil {
			w.onAnchor(anchors)
		}
	} else {
		log.Printf("orchd: command log: storing %d record(s) failed: %v", len(batch), err)
		w.mu.Lock()
		for dk, d := range gaps {
			w.drops[dk] += d
		}
		for _, j := range batch {
			if j.key.Owner == (Owner{}) {
				continue
			}
			dk := dropKey{j.key.Owner, j.key.Claim}
			w.drops[dk] += j.gap
			if !j.end {
				w.drops[dk]++
				w.dropped.Add(1)
			}
		}
		w.mu.Unlock()
	}
	for _, j := range batch {
		if j.done != nil {
			j.done <- err
		}
	}
}

// build runs j.build. A panic there would end orchd, since it runs on the
// writer goroutine; the event is stored with what the job knew instead.
func build(j job) (r Record) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("orchd: command log: building a record for %s panicked: %v", j.key.Owner, p)
			r = Record{TS: j.at.UnixNano(), Command: "[the record could not be built]"}
		}
	}()
	return j.build()
}

// chainOf is the chain k's events go to.
func (w *Writer) chainOf(k Key) Owner {
	if k.Claim == "" || w.resolve == nil {
		return k.Owner
	}
	if o := w.resolve(k); o.Kind == k.Owner.Kind && o.ID != "" {
		return o
	}
	return k.Owner
}

func (w *Writer) takeDrops() map[dropKey]int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.drops) == 0 {
		return nil
	}
	d := w.drops
	w.drops = map[dropKey]int64{}
	return d
}

// inTx runs fn in one write transaction.
func (s *Store) inTx(ctx context.Context, fn func(execer) error) error {
	return s.write(ctx, func(tx *sql.Tx) error { return fn(tx) })
}
