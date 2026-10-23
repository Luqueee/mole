package transfer

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Directions of a transfer record.
const (
	DirSend = "send"
	DirRecv = "recv"
)

// Statuses of a finished transfer.
const (
	StatusOK       = "ok"
	StatusFailed   = "failed"
	StatusRejected = "rejected"
)

// Record is one finished file transfer.
type Record struct {
	ID      uint64 `json:"id"`
	Dir     string `json:"dir"` // DirSend | DirRecv
	Peer    string `json:"peer"`
	Name    string `json:"name"`
	Saved   string `json:"saved,omitempty"` // final name at the destination
	Size    int64  `json:"size"`
	Bytes   int64  `json:"bytes"`
	StartMs int64  `json:"start_ms"`
	DurMs   int64  `json:"dur_ms"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
}

// Active is a file transfer in progress.
type Active struct {
	ID      uint64 `json:"id"`
	Dir     string `json:"dir"`
	Peer    string `json:"peer"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Bytes   int64  `json:"bytes"`
	StartMs int64  `json:"start_ms"`
}

const maxRecords = 200

// Log keeps the recent transfers and the ones in progress. Safe for
// concurrent use; a nil *Log and a nil *Tracker are valid no-ops.
type Log struct {
	mu      sync.Mutex
	records []Record
	next    uint64
	active  map[uint64]*Tracker
}

// NewLog returns an empty Log.
func NewLog() *Log { return &Log{active: map[uint64]*Tracker{}} }

// Tracker follows one file in progress. It is an io.Writer so it can be
// teed into the copy: every byte written counts as progress.
type Tracker struct {
	log   *Log
	id    uint64
	dir   string
	peer  string
	name  string
	size  int64
	start time.Time
	bytes atomic.Int64
}

// Begin registers a transfer in progress.
func (l *Log) Begin(dir, peer, name string, size int64) *Tracker {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	t := &Tracker{log: l, id: l.next, dir: dir, peer: peer, name: name, size: size, start: time.Now()}
	l.active[t.id] = t
	return t
}

// Write counts n bytes of progress.
func (t *Tracker) Write(p []byte) (int, error) {
	if t != nil {
		t.bytes.Add(int64(len(p)))
	}
	return len(p), nil
}

// Done finishes the transfer with status (and err, if it failed).
func (t *Tracker) Done(status string, err error, saved string) {
	if t == nil {
		return
	}
	rec := Record{
		ID: t.id, Dir: t.dir, Peer: t.peer, Name: t.name, Saved: saved, Size: t.size,
		Bytes: t.bytes.Load(), StartMs: t.start.UnixMilli(), DurMs: time.Since(t.start).Milliseconds(), Status: status,
	}
	if err != nil {
		rec.Error = err.Error()
	}
	l := t.log
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.active, t.id)
	l.append(rec)
}

func (l *Log) append(rec Record) {
	if len(l.records) >= maxRecords {
		l.records = append(l.records[:0], l.records[1:]...)
	}
	l.records = append(l.records, rec)
}

// Add records a transfer that never started, such as a refused offer.
func (l *Log) Add(rec Record) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	rec.ID = l.next
	l.append(rec)
}

// Since returns the records finished after ID since, the transfers in
// progress, and the newest ID issued so far.
func (l *Log) Since(since uint64) ([]Record, []Active, uint64) {
	recs, act := []Record{}, []Active{}
	if l == nil {
		return recs, act, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.records {
		if r.ID > since {
			recs = append(recs, r)
		}
	}
	for _, t := range l.active {
		act = append(act, Active{ID: t.id, Dir: t.dir, Peer: t.peer, Name: t.name, Size: t.size, Bytes: t.bytes.Load(), StartMs: t.start.UnixMilli()})
	}
	sort.Slice(act, func(i, j int) bool { return act[i].ID < act[j].ID })
	return recs, act, l.next
}
