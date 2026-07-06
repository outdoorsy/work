package work

import (
	"sync"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/robfig/cron"
	"github.com/stretchr/testify/assert"
)

func TestPeriodicEnqueuer(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"
	cleanKeyspace(ns, pool)

	var pjs []*periodicJob
	pjs = appendPeriodicJob(pjs, "0/29 * * * * *", "foo") // Every 29 seconds
	pjs = appendPeriodicJob(pjs, "3/49 * * * * *", "bar") // Every 49 seconds
	pjs = appendPeriodicJob(pjs, "* * * 2 * *", "baz")    // Every 2nd of the month seconds

	setNowEpochSecondsMock(1468359453)
	defer resetNowEpochSecondsMock()

	pe := newPeriodicEnqueuer(ns, pool, pjs)
	err := pe.enqueue()
	assert.NoError(t, err)

	c := NewClient(ns, pool)
	scheduledJobs, count, err := c.ScheduledJobs(1)
	assert.NoError(t, err)
	assert.EqualValues(t, 20, count)

	expected := []struct {
		name         string
		id           string
		scheduledFor int64
	}{
		{name: "bar", id: "periodic:bar:3/49 * * * * *:1468359472", scheduledFor: 1468359472},
		{name: "foo", id: "periodic:foo:0/29 * * * * *:1468359478", scheduledFor: 1468359478},
		{name: "foo", id: "periodic:foo:0/29 * * * * *:1468359480", scheduledFor: 1468359480},
		{name: "bar", id: "periodic:bar:3/49 * * * * *:1468359483", scheduledFor: 1468359483},
		{name: "foo", id: "periodic:foo:0/29 * * * * *:1468359509", scheduledFor: 1468359509},
		{name: "bar", id: "periodic:bar:3/49 * * * * *:1468359532", scheduledFor: 1468359532},
		{name: "foo", id: "periodic:foo:0/29 * * * * *:1468359538", scheduledFor: 1468359538},
		{name: "foo", id: "periodic:foo:0/29 * * * * *:1468359540", scheduledFor: 1468359540},
		{name: "bar", id: "periodic:bar:3/49 * * * * *:1468359543", scheduledFor: 1468359543},
		{name: "foo", id: "periodic:foo:0/29 * * * * *:1468359569", scheduledFor: 1468359569},
		{name: "bar", id: "periodic:bar:3/49 * * * * *:1468359592", scheduledFor: 1468359592},
		{name: "foo", id: "periodic:foo:0/29 * * * * *:1468359598", scheduledFor: 1468359598},
		{name: "foo", id: "periodic:foo:0/29 * * * * *:1468359600", scheduledFor: 1468359600},
		{name: "bar", id: "periodic:bar:3/49 * * * * *:1468359603", scheduledFor: 1468359603},
		{name: "foo", id: "periodic:foo:0/29 * * * * *:1468359629", scheduledFor: 1468359629},
		{name: "bar", id: "periodic:bar:3/49 * * * * *:1468359652", scheduledFor: 1468359652},
		{name: "foo", id: "periodic:foo:0/29 * * * * *:1468359658", scheduledFor: 1468359658},
		{name: "foo", id: "periodic:foo:0/29 * * * * *:1468359660", scheduledFor: 1468359660},
		{name: "bar", id: "periodic:bar:3/49 * * * * *:1468359663", scheduledFor: 1468359663},
		{name: "foo", id: "periodic:foo:0/29 * * * * *:1468359689", scheduledFor: 1468359689},
	}

	for i, e := range expected {
		assert.EqualValues(t, scheduledJobs[i].RunAt, scheduledJobs[i].EnqueuedAt)
		assert.Nil(t, scheduledJobs[i].Args)

		assert.Equal(t, e.name, scheduledJobs[i].Name)
		assert.Equal(t, e.id, scheduledJobs[i].ID)
		assert.Equal(t, e.scheduledFor, scheduledJobs[i].RunAt)
	}

	// shouldEnqueue() is what actually claims (and stamps) the periodic
	// window now -- enqueue() no longer touches redisKeyLastPeriodicEnqueue,
	// since that read-check-write needs to happen atomically in one step to
	// avoid the race two racing processes used to hit.
	assert.True(t, pe.shouldEnqueue())

	conn := pool.Get()
	defer conn.Close()

	lastEnqueue, err := redis.Int64(conn.Do("GET", redisKeyLastPeriodicEnqueue(ns)))
	assert.NoError(t, err)
	assert.EqualValues(t, 1468359453, lastEnqueue)

	// Immediately after, a second caller (or the same one) must not be able
	// to claim the same window again.
	assert.False(t, pe.shouldEnqueue())

	setNowEpochSecondsMock(1468359454)

	// Now do it again, and make sure nothing happens!
	err = pe.enqueue()
	assert.NoError(t, err)

	_, count, err = c.ScheduledJobs(1)
	assert.NoError(t, err)
	assert.EqualValues(t, 20, count)

	// Still within the dedup window relative to the original 1468359453
	// claim, so this must still be false.
	assert.False(t, pe.shouldEnqueue())

	setNowEpochSecondsMock(1468359453 + int64(periodicEnqueuerSleep/time.Second) + 10)

	assert.True(t, pe.shouldEnqueue())

	lastEnqueue, err = redis.Int64(conn.Do("GET", redisKeyLastPeriodicEnqueue(ns)))
	assert.NoError(t, err)
	assert.EqualValues(t, 1468359453+int64(periodicEnqueuerSleep/time.Second)+10, lastEnqueue)
}

func TestPeriodicEnqueuerSpawn(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"
	cleanKeyspace(ns, pool)

	pe := newPeriodicEnqueuer(ns, pool, nil)
	pe.start()
	pe.stop()
}

// TestPeriodicEnqueuerShouldEnqueueFailsOpen asserts that when Redis is
// unreachable, shouldEnqueue() fails open (returns true) rather than
// silently skipping periodic jobs forever. This is the same fail-open
// behavior the old GET-based implementation had; the switch to a Lua
// script must not regress it.
func TestPeriodicEnqueuerShouldEnqueueFailsOpen(t *testing.T) {
	pool := newTestPool("notworking:6379")
	ns := "work"

	pe := newPeriodicEnqueuer(ns, pool, nil)

	assert.True(t, pe.shouldEnqueue())
}

// TestPeriodicEnqueuerTryEnqueueReleasesClaimOnFailure guards against a
// regression where claiming the window before enqueue() runs could turn a
// single transient enqueue() failure into a permanently skipped occurrence
// for any job scheduled more often than the claim window: enqueue()'s
// scheduling only looks forward from "now", so if the window stayed held
// for its full duration after a failed attempt, the next successful
// attempt's forward-looking scan would never go back and pick up whatever
// should have fired in between.
func TestPeriodicEnqueuerTryEnqueueReleasesClaimOnFailure(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"
	cleanKeyspace(ns, pool)

	var pjs []*periodicJob
	pjs = appendPeriodicJob(pjs, "0/29 * * * * *", "foo")

	setNowEpochSecondsMock(1468359453)
	defer resetNowEpochSecondsMock()

	conn := pool.Get()
	// Force enqueue()'s ZADD to fail with a real Redis error by making the
	// scheduled-jobs key the wrong type -- this exercises the actual
	// failure path through a live connection, not a broken pool (which
	// would make shouldEnqueue() fail open before ever claiming anything).
	_, err := conn.Do("SET", redisKeyScheduled(ns), "not-a-sorted-set")
	assert.NoError(t, err)
	conn.Close()

	pe := newPeriodicEnqueuer(ns, pool, pjs)

	pe.tryEnqueue()

	// The claim must have been released on failure, not held for the rest
	// of the window, so the very next attempt can retry immediately.
	assert.True(t, pe.shouldEnqueue())
}

// TestPeriodicEnqueuerReleaseClaimDoesNotStealANewerClaim guards against a
// narrower edge case than the one above: if this process's own claim has
// already expired and a *different* process has since claimed the window,
// this process calling releaseClaim() (e.g. because its own long-delayed
// enqueue() attempt finally errors out) must not delete that other,
// currently-valid claim out from under it. releaseClaim() must only ever
// release the exact claim this process itself made.
func TestPeriodicEnqueuerReleaseClaimDoesNotStealANewerClaim(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"
	cleanKeyspace(ns, pool)

	setNowEpochSecondsMock(1468359453)
	defer resetNowEpochSecondsMock()

	pe := newPeriodicEnqueuer(ns, pool, nil)
	assert.True(t, pe.shouldEnqueue()) // pe claims at 1468359453

	// Simulate another process's claim expiring and being re-claimed later,
	// long after pe's own (now-stale) claim.
	conn := pool.Get()
	newerClaim := int64(1468359453 + 500)
	_, err := conn.Do("SET", redisKeyLastPeriodicEnqueue(ns), newerClaim)
	assert.NoError(t, err)
	conn.Close()

	// pe's enqueue() finally fails and it tries to release its own
	// (long-expired) claim -- this must not touch the newer one.
	pe.releaseClaim()

	conn = pool.Get()
	defer conn.Close()
	current, err := redis.Int64(conn.Do("GET", redisKeyLastPeriodicEnqueue(ns)))
	assert.NoError(t, err)
	assert.EqualValues(t, newerClaim, current)
}

// TestPeriodicEnqueuerShouldEnqueueRace guards against the check-then-act
// race the old GET-then-SET implementation had: many periodicEnqueuer
// instances (standing in for many worker pool processes sharing one Redis
// namespace) call shouldEnqueue() concurrently for the same window, and
// exactly one of them may win the claim.
func TestPeriodicEnqueuerShouldEnqueueRace(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"
	cleanKeyspace(ns, pool)

	setNowEpochSecondsMock(1468359453)
	defer resetNowEpochSecondsMock()

	const contenders = 20
	claims := make(chan bool, contenders)
	var wg sync.WaitGroup

	for range contenders {
		pe := newPeriodicEnqueuer(ns, pool, nil)
		wg.Add(1)
		go func() {
			defer wg.Done()
			claims <- pe.shouldEnqueue()
		}()
	}
	wg.Wait()
	close(claims)

	winners := 0
	for claimed := range claims {
		if claimed {
			winners++
		}
	}

	assert.Equal(t, 1, winners)
}

func appendPeriodicJob(pjs []*periodicJob, spec, jobName string) []*periodicJob {
	sched, err := cron.Parse(spec)
	if err != nil {
		panic(err)
	}
	pj := &periodicJob{jobName: jobName, spec: spec, schedule: sched}
	return append(pjs, pj)
}
