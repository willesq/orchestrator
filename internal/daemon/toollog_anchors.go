package daemon

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/willesq/orchestrator/internal/toollog"
)

// maxPendingCopies bounds the anchors and cuts kept in memory while orch.db
// cannot take them; past it the oldest are dropped (and logged), and verify
// reports them as missing from orch.db.
const maxPendingCopies = 10000

// copyTimeout bounds one write of the copies into orch.db.
const copyTimeout = 10 * time.Second

// logCopies keeps a copy of every command log anchor, and of every pruned
// head retention moves, in orch.db (D-35): toollog.db holds a chain's
// anchors and pruned head beside its records, so whoever can rewrite one
// can rewrite the others; verify compares the two stores. The writer and
// retention hand their results over without waiting; one goroutine writes
// them, so a slow orch.db never stalls the command log's writer.
type logCopies struct {
	db *sql.DB
	mu sync.Mutex
	// anchors and cuts wait to be written; a failed write puts them back.
	anchors []toollog.Anchor
	cuts    []toollog.Cut
	// flushMu makes writes take turns, so a verify's flush sees every
	// copy handed over before it.
	flushMu  sync.Mutex
	kick     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

func newLogCopies(db *sql.DB) *logCopies {
	return &logCopies{db: db, kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
}

// start runs the goroutine that writes handed-over copies until close.
func (c *logCopies) start() {
	go func() {
		defer close(c.done)
		for {
			select {
			case <-c.kick:
				c.flush()
			case <-c.stop:
				c.flush()
				return
			}
		}
	}()
}

// close writes what is waiting and stops the goroutine; later calls wait
// for the first.
func (c *logCopies) close() {
	c.stopOnce.Do(func() { close(c.stop) })
	<-c.done
}

// addAnchors hands over anchors the writer committed; it never blocks on
// orch.db.
func (c *logCopies) addAnchors(anchors []toollog.Anchor) {
	c.mu.Lock()
	c.anchors = bounded(append(c.anchors, anchors...), "anchor")
	c.mu.Unlock()
	c.wake()
}

// addCut hands over a pruned head retention committed.
func (c *logCopies) addCut(k toollog.Cut) {
	c.mu.Lock()
	c.cuts = bounded(append(c.cuts, k), "pruned head")
	c.mu.Unlock()
	c.wake()
}

// wake has the goroutine write what waits; once it has stopped (a sweep
// that outlived orchd's shutdown), the caller writes it.
func (c *logCopies) wake() {
	select {
	case <-c.stop:
		c.flush()
		return
	default:
	}
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// bounded drops the oldest of items past maxPendingCopies.
func bounded[T any](items []T, what string) []T {
	if over := len(items) - maxPendingCopies; over > 0 {
		log.Printf("orchd: command log: dropping %d %s copies orch.db could not take", over, what)
		return items[over:]
	}
	return items
}

// flush writes every waiting copy; a failure keeps them for the next flush
// (the next hand-over, retention sweep or verify).
func (c *logCopies) flush() {
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	c.mu.Lock()
	anchors, cuts := c.anchors, c.cuts
	c.anchors, c.cuts = nil, nil
	c.mu.Unlock()
	if len(anchors)+len(cuts) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), copyTimeout)
	defer cancel()
	if err := c.write(ctx, anchors, cuts); err != nil {
		log.Printf("orchd: command log: copying %d anchor(s) and %d pruned head(s) into orch.db: %v; retrying later",
			len(anchors), len(cuts), err)
		c.mu.Lock()
		c.anchors = bounded(append(anchors, c.anchors...), "anchor")
		c.cuts = bounded(append(cuts, c.cuts...), "pruned head")
		c.mu.Unlock()
	}
}

// write stores anchors and cuts in one transaction. An anchor already
// copied is never replaced: a copy that disagrees is logged, and verify
// reports it. A pruned head only moves forward, as in toollog.db.
func (c *logCopies) write(ctx context.Context, anchors []toollog.Anchor, cuts []toollog.Cut) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, a := range anchors {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO toollog_anchors (owner_kind, owner_id, seq, hash, at) VALUES (?,?,?,?,?)
			ON CONFLICT (owner_kind, owner_id, seq) DO NOTHING`,
			a.Owner.Kind, a.Owner.ID, a.Seq, a.Hash, a.At)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			var held string
			if err := tx.QueryRowContext(ctx, `SELECT hash FROM toollog_anchors WHERE owner_kind = ? AND owner_id = ? AND seq = ?`,
				a.Owner.Kind, a.Owner.ID, a.Seq).Scan(&held); err == nil && held != a.Hash {
				log.Printf("orchd: command log: anchor %d of %s in toollog.db differs from orch.db's copy; kept the copy",
					a.Seq, printableID(a.Owner.String()))
			}
		}
	}
	for _, k := range cuts {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO toollog_cuts (owner_kind, owner_id, seq, hash) VALUES (?,?,?,?)
			ON CONFLICT (owner_kind, owner_id) DO UPDATE SET seq = excluded.seq, hash = excluded.hash
			WHERE excluded.seq > toollog_cuts.seq`,
			k.Owner.Kind, k.Owner.ID, k.Seq, k.Hash); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// of returns what orch.db holds of o's chain, after writing every copy
// handed over before the call.
func (c *logCopies) of(ctx context.Context, o toollog.Owner) (toollog.Copies, error) {
	c.flush()
	out := toollog.Copies{Anchors: []toollog.Anchor{}}
	rows, err := c.db.QueryContext(ctx, `SELECT seq, hash, at FROM toollog_anchors
		WHERE owner_kind = ? AND owner_id = ? ORDER BY seq`, o.Kind, o.ID)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		a := toollog.Anchor{Owner: o}
		if err := rows.Scan(&a.Seq, &a.Hash, &a.At); err != nil {
			return out, err
		}
		out.Anchors = append(out.Anchors, a)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	err = c.db.QueryRowContext(ctx, `SELECT seq, hash FROM toollog_cuts WHERE owner_kind = ? AND owner_id = ?`,
		o.Kind, o.ID).Scan(&out.Cut.Seq, &out.Cut.Hash)
	if err == sql.ErrNoRows {
		err = nil
	}
	out.Cut.Owner = o
	return out, err
}

// copyAtOpen runs when orchd opens the log, before its writer starts and
// before any sweep. Anchors toollog.db holds that orch.db lacks are copied
// (a crash can fall between the two writes; an anchor added by hand only
// constrains the chain further), and copies orch.db holds are never
// replaced. Pruned heads are copied only the first time, when orch.db has
// none yet (the log predates SP-073): copying them later would let a head
// written into the file by hand excuse the records it claims retention
// deleted.
func (c *logCopies) copyAtOpen(ctx context.Context, st *toollog.Store) error {
	anchors, err := st.Anchors(ctx)
	if err != nil {
		return err
	}
	var started int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM toollog_copy_started`).Scan(&started); err != nil {
		return err
	}
	var cuts []toollog.Cut
	if started == 0 {
		if cuts, err = st.Cuts(ctx); err != nil {
			return err
		}
	}
	before, err := c.count(ctx)
	if err != nil {
		return err
	}
	if err := c.write(ctx, anchors, cuts); err != nil {
		return err
	}
	after, err := c.count(ctx)
	if err != nil {
		return err
	}
	if after > before {
		log.Printf("orchd: command log: copied %d anchor(s) orch.db lacked from toollog.db", after-before)
	}
	if started == 0 {
		_, err = c.db.ExecContext(ctx, `INSERT OR IGNORE INTO toollog_copy_started (id, at) VALUES (1, ?)`,
			time.Now().UTC().Format(time.RFC3339))
	}
	return err
}

func (c *logCopies) count(ctx context.Context) (n int64, err error) {
	err = c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM toollog_anchors`).Scan(&n)
	return n, err
}
