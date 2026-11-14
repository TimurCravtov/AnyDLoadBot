package bot

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

var (
	errUserLimit = errors.New("per-user download limit reached")
	errBusy      = errors.New("global download limit reached")
)

// jobs tracks the downloads that are currently running, to enforce limits and
// let users cancel them. A job holds a slot from start until finish, including
// the time it waits for a free yt-dlp process.
type jobs struct {
	maxUser, maxTotal int

	mu     sync.Mutex
	total  int
	byUser map[int64]map[*job]struct{}
	byKey  map[string]*job
}

func newJobs(maxUser, maxTotal int) *jobs {
	return &jobs{
		maxUser: maxUser, maxTotal: maxTotal,
		byUser: make(map[int64]map[*job]struct{}),
		byKey:  make(map[string]*job),
	}
}

// job is one running download.
type job struct {
	reg    *jobs
	user   int64
	ctx    context.Context
	cancel context.CancelFunc
	key    string // identifies the status message with the Cancel button, if any

	cancelled atomic.Bool // set only when the user cancels, not on timeouts
}

// start reserves a slot for user. It returns errUserLimit or errBusy when a
// limit is reached. The job's context is cancelled when the user cancels it,
// when parent is cancelled, or on finish.
func (r *jobs) start(parent context.Context, user int64) (*job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// The per-user limit is checked first: it is the one the user can do something about.
	if len(r.byUser[user]) >= r.maxUser {
		return nil, errUserLimit
	}
	if r.total >= r.maxTotal {
		return nil, errBusy
	}

	ctx, cancel := context.WithCancel(parent)
	j := &job{reg: r, user: user, ctx: ctx, cancel: cancel}
	if r.byUser[user] == nil {
		r.byUser[user] = make(map[*job]struct{})
	}
	r.byUser[user][j] = struct{}{}
	r.total++
	return j, nil
}

// finish releases the job's slot. It is safe to call more than once.
func (j *job) finish() {
	r := j.reg
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.byUser[j.user][j]; !ok {
		return
	}
	delete(r.byUser[j.user], j)
	if len(r.byUser[j.user]) == 0 {
		delete(r.byUser, j.user)
	}
	if j.key != "" {
		delete(r.byKey, j.key)
	}
	r.total--
	j.cancel()
}

// setKey makes the job cancellable by the Cancel button on the message with this key.
func (j *job) setKey(key string) {
	r := j.reg
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.byUser[j.user][j]; !ok {
		return // already finished
	}
	j.key = key
	r.byKey[key] = j
}

// wasCancelled reports whether the user cancelled this job.
func (j *job) wasCancelled() bool { return j.cancelled.Load() }

// cancelKey cancels the job behind a Cancel button. Only its owner may cancel it.
func (r *jobs) cancelKey(user int64, key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	j := r.byKey[key]
	if j == nil || j.user != user {
		return false
	}
	j.cancelled.Store(true)
	j.cancel()
	return true
}

// cancelUser cancels all of a user's running downloads and returns how many there were.
func (r *jobs) cancelUser(user int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := 0
	for j := range r.byUser[user] {
		j.cancelled.Store(true)
		j.cancel()
		n++
	}
	return n
}

// byUserCount is how many downloads a user has running.
func (r *jobs) byUserCount(user int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byUser[user])
}
