package bot

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestJobsPerUserLimit(t *testing.T) {
	t.Parallel()

	r := newJobs(2, 100)
	ctx := context.Background()

	a, err := r.start(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.start(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.start(ctx, 1); !errors.Is(err, errUserLimit) {
		t.Fatalf("third job: err = %v, want errUserLimit", err)
	}

	// Another user is unaffected.
	if _, err := r.start(ctx, 2); err != nil {
		t.Errorf("other user: %v", err)
	}

	// Finishing a job frees a slot, and finishing twice must not free two.
	a.finish()
	a.finish()
	if _, err := r.start(ctx, 1); err != nil {
		t.Fatalf("after finish: %v", err)
	}
	if _, err := r.start(ctx, 1); !errors.Is(err, errUserLimit) {
		t.Errorf("double finish freed an extra slot: err = %v", err)
	}
}

func TestJobsGlobalLimit(t *testing.T) {
	t.Parallel()

	r := newJobs(5, 3)
	ctx := context.Background()

	var held []*job
	for user := int64(1); user <= 3; user++ {
		j, err := r.start(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, j)
	}
	if _, err := r.start(ctx, 4); !errors.Is(err, errBusy) {
		t.Fatalf("err = %v, want errBusy", err)
	}

	// A user at their own limit is told so, not that the whole bot is busy.
	r2 := newJobs(1, 1)
	if _, err := r2.start(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.start(ctx, 1); !errors.Is(err, errUserLimit) {
		t.Errorf("err = %v, want errUserLimit", err)
	}

	held[0].finish()
	if _, err := r.start(ctx, 4); err != nil {
		t.Errorf("after a slot freed: %v", err)
	}
}

func TestJobsCancel(t *testing.T) {
	t.Parallel()

	r := newJobs(5, 100)
	j, err := r.start(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	j.setKey("chat:1")

	// Someone else can't cancel it, and an unknown key does nothing.
	if r.cancelKey(8, "chat:1") || r.cancelKey(7, "chat:2") {
		t.Fatal("cancelled by the wrong user or key")
	}
	if j.ctx.Err() != nil || j.wasCancelled() {
		t.Fatal("job was cancelled by a rejected request")
	}

	if !r.cancelKey(7, "chat:1") {
		t.Fatal("owner could not cancel")
	}
	if j.ctx.Err() == nil || !j.wasCancelled() {
		t.Errorf("job not cancelled: ctx=%v cancelled=%v", j.ctx.Err(), j.wasCancelled())
	}
	j.finish()
	if r.cancelKey(7, "chat:1") {
		t.Error("a finished job can still be cancelled")
	}
}

func TestJobsFinishIsNotACancel(t *testing.T) {
	t.Parallel()

	j, err := newJobs(5, 100).start(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	j.finish()
	if j.ctx.Err() == nil {
		t.Error("finish must release the context")
	}
	if j.wasCancelled() {
		t.Error("a normal finish must not look like a user cancel")
	}
}

func TestJobsCancelUser(t *testing.T) {
	t.Parallel()

	r := newJobs(5, 100)
	ctx := context.Background()
	a, _ := r.start(ctx, 1)
	b, _ := r.start(ctx, 1)
	other, _ := r.start(ctx, 2)

	if n := r.cancelUser(1); n != 2 {
		t.Errorf("cancelUser = %d, want 2", n)
	}
	if !a.wasCancelled() || !b.wasCancelled() {
		t.Error("user's jobs were not cancelled")
	}
	if other.ctx.Err() != nil {
		t.Error("another user's job was cancelled")
	}
	if n := r.cancelUser(3); n != 0 {
		t.Errorf("cancelUser(no jobs) = %d, want 0", n)
	}
}

func TestJobsConcurrent(t *testing.T) {
	t.Parallel()

	r := newJobs(5, 20)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(user int64) {
			defer wg.Done()
			j, err := r.start(context.Background(), user)
			if err != nil {
				return
			}
			j.setKey("k")
			r.cancelUser(user)
			j.finish()
		}(int64(i % 7))
	}
	wg.Wait()

	if r.total != 0 || len(r.byUser) != 0 {
		t.Errorf("leaked jobs: total=%d users=%d", r.total, len(r.byUser))
	}
}
