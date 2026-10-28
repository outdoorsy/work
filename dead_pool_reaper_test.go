package work

import (
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/stretchr/testify/assert"
)

func TestDeadPoolReaper(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"
	cleanKeyspace(ns, pool)

	conn := pool.Get()
	defer conn.Close()

	workerPoolsKey := redisKeyWorkerPools(ns)

	// Create redis data
	var err error
	err = conn.Send("SADD", workerPoolsKey, "1")
	assert.NoError(t, err)
	err = conn.Send("SADD", workerPoolsKey, "2")
	assert.NoError(t, err)
	err = conn.Send("SADD", workerPoolsKey, "3")
	assert.NoError(t, err)

	err = conn.Send("HMSET", redisKeyHeartbeat(ns, "1"),
		"heartbeat_at", time.Now().Unix(),
		"job_names", "type1,type2",
	)
	assert.NoError(t, err)

	err = conn.Send("HMSET", redisKeyHeartbeat(ns, "2"),
		"heartbeat_at", time.Now().Add(-1*time.Hour).Unix(),
		"job_names", "type1,type2",
	)
	assert.NoError(t, err)

	err = conn.Send("HMSET", redisKeyHeartbeat(ns, "3"),
		"heartbeat_at", time.Now().Add(-1*time.Hour).Unix(),
		"job_names", "type1,type2",
	)
	assert.NoError(t, err)
	err = conn.Flush()
	assert.NoError(t, err)

	// Test getting dead pool
	reaper := newDeadPoolReaper(ns, pool, []string{})
	deadPools, err := reaper.findDeadPools()
	assert.NoError(t, err)
	assert.Equal(t, map[string][]string{"2": {"type1", "type2"}, "3": {"type1", "type2"}}, deadPools)

	// Test requeueing jobs
	_, err = conn.Do("lpush", redisKeyJobsInProgress(ns, "2", "type1"), "foo")
	assert.NoError(t, err)
	_, err = conn.Do("incr", redisKeyJobsLock(ns, "type1"))
	assert.NoError(t, err)
	_, err = conn.Do("hincrby", redisKeyJobsLockInfo(ns, "type1"), "2", 1) // worker pool 2 has lock
	assert.NoError(t, err)

	// Ensure 0 jobs in jobs queue
	jobsCount, err := redis.Int(conn.Do("llen", redisKeyJobs(ns, "type1")))
	assert.NoError(t, err)
	assert.Equal(t, 0, jobsCount)

	// Ensure 1 job in inprogress queue
	jobsCount, err = redis.Int(conn.Do("llen", redisKeyJobsInProgress(ns, "2", "type1")))
	assert.NoError(t, err)
	assert.Equal(t, 1, jobsCount)

	// Reap
	err = reaper.reap()
	assert.NoError(t, err)

	// Ensure 1 jobs in jobs queue
	jobsCount, err = redis.Int(conn.Do("llen", redisKeyJobs(ns, "type1")))
	assert.NoError(t, err)
	assert.Equal(t, 1, jobsCount)

	// Ensure 0 job in inprogress queue
	jobsCount, err = redis.Int(conn.Do("llen", redisKeyJobsInProgress(ns, "2", "type1")))
	assert.NoError(t, err)
	assert.Equal(t, 0, jobsCount)

	// Locks should get cleaned up
	assert.EqualValues(t, 0, getInt64(pool, redisKeyJobsLock(ns, "type1")))
	v, _ := conn.Do("HGET", redisKeyJobsLockInfo(ns, "type1"), "2")
	assert.Nil(t, v)
}

func TestDeadPoolReaperNoHeartbeat(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"

	conn := pool.Get()
	defer conn.Close()

	workerPoolsKey := redisKeyWorkerPools(ns)

	// Create redis data
	var err error
	cleanKeyspace(ns, pool)
	err = conn.Send("SADD", workerPoolsKey, "1")
	assert.NoError(t, err)
	err = conn.Send("SADD", workerPoolsKey, "2")
	assert.NoError(t, err)
	err = conn.Send("SADD", workerPoolsKey, "3")
	assert.NoError(t, err)
	// stale lock info
	err = conn.Send("SET", redisKeyJobsLock(ns, "type1"), 3)
	assert.NoError(t, err)
	err = conn.Send("HSET", redisKeyJobsLockInfo(ns, "type1"), "1", 1)
	assert.NoError(t, err)
	err = conn.Send("HSET", redisKeyJobsLockInfo(ns, "type1"), "2", 1)
	assert.NoError(t, err)
	err = conn.Send("HSET", redisKeyJobsLockInfo(ns, "type1"), "3", 1)
	assert.NoError(t, err)
	err = conn.Flush()
	assert.NoError(t, err)

	// make sure test data was created
	numPools, err := redis.Int(conn.Do("scard", workerPoolsKey))
	assert.NoError(t, err)
	assert.EqualValues(t, 3, numPools)

	// Test getting dead pool ids
	reaper := newDeadPoolReaper(ns, pool, []string{"type1"})
	deadPools, err := reaper.findDeadPools()
	assert.NoError(t, err)
	assert.Equal(t, map[string][]string{"1": {}, "2": {}, "3": {}}, deadPools)

	// Test requeueing jobs
	_, err = conn.Do("lpush", redisKeyJobsInProgress(ns, "2", "type1"), "foo")
	assert.NoError(t, err)

	// Ensure 0 jobs in jobs queue
	jobsCount, err := redis.Int(conn.Do("llen", redisKeyJobs(ns, "type1")))
	assert.NoError(t, err)
	assert.Equal(t, 0, jobsCount)

	// Ensure 1 job in inprogress queue
	jobsCount, err = redis.Int(conn.Do("llen", redisKeyJobsInProgress(ns, "2", "type1")))
	assert.NoError(t, err)
	assert.Equal(t, 1, jobsCount)

	// Ensure dead worker pools still in the set
	jobsCount, err = redis.Int(conn.Do("scard", redisKeyWorkerPools(ns)))
	assert.NoError(t, err)
	assert.Equal(t, 3, jobsCount)

	// Reap
	err = reaper.reap()
	assert.NoError(t, err)

	// Ensure jobs queue was not altered
	jobsCount, err = redis.Int(conn.Do("llen", redisKeyJobs(ns, "type1")))
	assert.NoError(t, err)
	assert.Equal(t, 0, jobsCount)

	// Ensure inprogress queue was not altered
	jobsCount, err = redis.Int(conn.Do("llen", redisKeyJobsInProgress(ns, "2", "type1")))
	assert.NoError(t, err)
	assert.Equal(t, 1, jobsCount)

	// Ensure dead worker pools were removed from the set
	jobsCount, err = redis.Int(conn.Do("scard", redisKeyWorkerPools(ns)))
	assert.NoError(t, err)
	assert.Equal(t, 0, jobsCount)

	// Stale lock info was cleaned up using reap.curJobTypes
	assert.EqualValues(t, 0, getInt64(pool, redisKeyJobsLock(ns, "type1")))
	for _, poolID := range []string{"1", "2", "3"} {
		v, _ := conn.Do("HGET", redisKeyJobsLockInfo(ns, "type1"), poolID)
		assert.Nil(t, v)
	}
}

func TestDeadPoolReaperNoJobTypes(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"
	cleanKeyspace(ns, pool)

	conn := pool.Get()
	defer conn.Close()

	workerPoolsKey := redisKeyWorkerPools(ns)

	// Create redis data
	var err error
	err = conn.Send("SADD", workerPoolsKey, "1")
	assert.NoError(t, err)
	err = conn.Send("SADD", workerPoolsKey, "2")
	assert.NoError(t, err)

	err = conn.Send("HMSET", redisKeyHeartbeat(ns, "1"),
		"heartbeat_at", time.Now().Add(-1*time.Hour).Unix(),
	)
	assert.NoError(t, err)

	err = conn.Send("HMSET", redisKeyHeartbeat(ns, "2"),
		"heartbeat_at", time.Now().Add(-1*time.Hour).Unix(),
		"job_names", "type1,type2",
	)
	assert.NoError(t, err)

	err = conn.Flush()
	assert.NoError(t, err)

	// Test getting dead pool
	reaper := newDeadPoolReaper(ns, pool, []string{})
	deadPools, err := reaper.findDeadPools()
	assert.NoError(t, err)
	assert.Equal(t, map[string][]string{"2": {"type1", "type2"}}, deadPools)

	// Test requeueing jobs
	_, err = conn.Do("lpush", redisKeyJobsInProgress(ns, "1", "type1"), "foo")
	assert.NoError(t, err)
	_, err = conn.Do("lpush", redisKeyJobsInProgress(ns, "2", "type1"), "foo")
	assert.NoError(t, err)

	// Ensure 0 jobs in jobs queue
	jobsCount, err := redis.Int(conn.Do("llen", redisKeyJobs(ns, "type1")))
	assert.NoError(t, err)
	assert.Equal(t, 0, jobsCount)

	// Ensure 1 job in inprogress queue for each job
	jobsCount, err = redis.Int(conn.Do("llen", redisKeyJobsInProgress(ns, "1", "type1")))
	assert.NoError(t, err)
	assert.Equal(t, 1, jobsCount)
	jobsCount, err = redis.Int(conn.Do("llen", redisKeyJobsInProgress(ns, "2", "type1")))
	assert.NoError(t, err)
	assert.Equal(t, 1, jobsCount)

	// Reap. Ensure job 2 is requeued but not job 1
	err = reaper.reap()
	assert.NoError(t, err)

	// Ensure 1 jobs in jobs queue
	jobsCount, err = redis.Int(conn.Do("llen", redisKeyJobs(ns, "type1")))
	assert.NoError(t, err)
	assert.Equal(t, 1, jobsCount)

	// Ensure 1 job in inprogress queue for 1
	jobsCount, err = redis.Int(conn.Do("llen", redisKeyJobsInProgress(ns, "1", "type1")))
	assert.NoError(t, err)
	assert.Equal(t, 1, jobsCount)

	// Ensure 0 jobs in inprogress queue for 2
	jobsCount, err = redis.Int(conn.Do("llen", redisKeyJobsInProgress(ns, "2", "type1")))
	assert.NoError(t, err)
	assert.Equal(t, 0, jobsCount)
}

func TestDeadPoolReaperWithWorkerPools(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"
	job1 := "job1"
	stalePoolID := "aaa"
	cleanKeyspace(ns, pool)
	// test vars
	expectedDeadTime := 5 * time.Millisecond

	// create a stale job with a heartbeat
	conn := pool.Get()
	defer conn.Close()
	_, err := conn.Do("SADD", redisKeyWorkerPools(ns), stalePoolID)
	assert.NoError(t, err)
	_, err = conn.Do("LPUSH", redisKeyJobsInProgress(ns, stalePoolID, job1), `{"sleep": 10}`)
	assert.NoError(t, err)
	jobTypes := map[string]*jobType{"job1": nil}
	staleHeart := newWorkerPoolHeartbeater(ns, pool, stalePoolID, jobTypes, 1, []string{"id1"})
	staleHeart.start()

	// should have 1 stale job and empty job queue
	assert.EqualValues(t, 1, listSize(pool, redisKeyJobsInProgress(ns, stalePoolID, job1)))
	assert.EqualValues(t, 0, listSize(pool, redisKeyJobs(ns, job1)))

	// setup a worker pool and start the reaper, which should restart the stale job above
	wp := setupTestWorkerPool(pool, ns, job1, 1, JobOptions{Priority: 1})
	wp.deadPoolReaper = newDeadPoolReaper(wp.namespace, wp.pool, []string{"job1"})
	wp.deadPoolReaper.deadTime = expectedDeadTime
	wp.deadPoolReaper.start()

	// sleep long enough for staleJob to be considered dead
	time.Sleep(expectedDeadTime * 2)

	// now we should have 1 job in queue and no more stale jobs
	assert.EqualValues(t, 1, listSize(pool, redisKeyJobs(ns, job1)))
	assert.EqualValues(t, 0, listSize(pool, redisKeyJobsInProgress(ns, wp.workerPoolID, job1)))
	staleHeart.stop()
	wp.deadPoolReaper.stop()
}

func TestDeadPoolReaperCleanStaleLocks(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"
	cleanKeyspace(ns, pool)

	conn := pool.Get()
	defer conn.Close()
	job1, job2 := "type1", "type2"
	jobNames := []string{job1, job2}
	workerPoolID1, workerPoolID2 := "1", "2"
	lock1 := redisKeyJobsLock(ns, job1)
	lock2 := redisKeyJobsLock(ns, job2)
	lockInfo1 := redisKeyJobsLockInfo(ns, job1)
	lockInfo2 := redisKeyJobsLockInfo(ns, job2)

	// Create redis data
	var err error
	err = conn.Send("SET", lock1, 3)
	assert.NoError(t, err)
	err = conn.Send("SET", lock2, 1)
	assert.NoError(t, err)
	err = conn.Send("HSET", lockInfo1, workerPoolID1, 1) // workerPoolID1 holds 1 lock on job1
	assert.NoError(t, err)
	err = conn.Send("HSET", lockInfo1, workerPoolID2, 2) // workerPoolID2 holds 2 locks on job1
	assert.NoError(t, err)
	err = conn.Send("HSET", lockInfo2, workerPoolID2, 2) // test that we don't go below 0 on job2 lock
	assert.NoError(t, err)
	err = conn.Flush()
	assert.NoError(t, err)

	reaper := newDeadPoolReaper(ns, pool, jobNames)
	// clean lock info for workerPoolID1
	reaper.cleanStaleLockInfo(workerPoolID1, jobNames)
	assert.NoError(t, err)
	assert.EqualValues(t, 2, getInt64(pool, lock1))   // job1 lock should be decr by 1
	assert.EqualValues(t, 1, getInt64(pool, lock2))   // job2 lock is unchanged
	v, _ := conn.Do("HGET", lockInfo1, workerPoolID1) // workerPoolID1 removed from job1's lock info
	assert.Nil(t, v)

	// now clean lock info for workerPoolID2
	reaper.cleanStaleLockInfo(workerPoolID2, jobNames)
	assert.NoError(t, err)
	// both locks should be at 0
	assert.EqualValues(t, 0, getInt64(pool, lock1))
	assert.EqualValues(t, 0, getInt64(pool, lock2))
	// worker pool ID 2 removed from both lock info hashes
	v, err = conn.Do("HGET", lockInfo1, workerPoolID2)
	assert.Nil(t, v)
	v, err = conn.Do("HGET", lockInfo2, workerPoolID2)
	assert.Nil(t, v)
}

func TestDeadPoolReaperOrphanedLock(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"
	job1 := "job1"
	cleanKeyspace(ns, pool)

	seedRedis(t, pool,
		cmd("SET", redisKeyJobsLock(ns, job1), 1),
		cmd("HSET", redisKeyJobsLockInfo(ns, job1), "gone", 1),
		cmd("LPUSH", redisKeyJobsInProgress(ns, "gone", job1), `{"name":"job1"}`),
		cmd("LPUSH", redisKeyJobs(ns, job1), `{"name":"job1"}`, `{"name":"job1"}`),
	)

	reaper := newDeadPoolReaper(ns, pool, []string{job1})
	assert.NoError(t, reaper.reap())
	assert.EqualValues(t, 1, lockCount(pool, redisKeyJobsLock(ns, job1)))
	assertLockInfo(t, pool, redisKeyJobsLockInfo(ns, job1), "gone", 1)

	assert.NoError(t, reaper.reap())
	assert.EqualValues(t, 0, lockCount(pool, redisKeyJobsLock(ns, job1)))
	assertNoLockInfo(t, pool, redisKeyJobsLockInfo(ns, job1), "gone")
	// lists are left as they are
	assert.EqualValues(t, 1, listSize(pool, redisKeyJobsInProgress(ns, "gone", job1)))
	assert.EqualValues(t, 2, listSize(pool, redisKeyJobs(ns, job1)))
}

func TestDeadPoolReaperOrphanedLockHeartbeat(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"
	job1 := "job1"
	lockKey, lockInfoKey := redisKeyJobsLock(ns, job1), redisKeyJobsLockInfo(ns, job1)
	cleanKeyspace(ns, pool)

	seedRedis(t, pool,
		cmd("SET", lockKey, 4),
		// live: in worker_pools with a fresh heartbeat
		cmd("SADD", redisKeyWorkerPools(ns), "live"),
		cmd("HSET", redisKeyHeartbeat(ns, "live"), "heartbeat_at", time.Now().Unix()),
		cmd("HSET", lockInfoKey, "live", 1),
		// starting: not in worker_pools yet, fresh heartbeat
		cmd("HSET", redisKeyHeartbeat(ns, "starting"), "heartbeat_at", time.Now().Unix()),
		cmd("HSET", lockInfoKey, "starting", 1),
		// stale: not in worker_pools, heartbeat key left behind
		cmd("HSET", redisKeyHeartbeat(ns, "stale"), "heartbeat_at", time.Now().Add(-time.Hour).Unix()),
		cmd("HSET", lockInfoKey, "stale", 1),
		// noat: not in worker_pools, heartbeat key without heartbeat_at
		cmd("HSET", redisKeyHeartbeat(ns, "noat"), "job_names", job1),
		cmd("HSET", lockInfoKey, "noat", 1),
	)

	reaper := newDeadPoolReaper(ns, pool, []string{job1})
	for i := 0; i < 3; i++ {
		assert.NoError(t, reaper.reap())
	}
	assert.EqualValues(t, 2, lockCount(pool, lockKey))
	assertLockInfo(t, pool, lockInfoKey, "live", 1)
	assertLockInfo(t, pool, lockInfoKey, "starting", 1)
	assertNoLockInfo(t, pool, lockInfoKey, "stale")
	assertNoLockInfo(t, pool, lockInfoKey, "noat")
}

func TestDeadPoolReaperOrphanedLockCounts(t *testing.T) {
	ns := "work"
	cases := []struct {
		name     string
		lock     interface{} // nil: no lock key
		orphan   int64
		live     int64 // count of a live pool, 0: none
		wantLock int64
	}{
		{"zero count", 3, 0, 0, 3},
		{"floor holds live locks", 1, 1, 1, 1},
		{"never raises", 0, 1, 1, 0},
		{"negative pays back up to the floor", 0, -1, 1, 1},
		{"negative with no live pool", 0, -1, 0, 0},
		{"negative never lowers", 2, -1, 1, 2},
		{"clamps a negative lock", -1, 1, 0, 0},
		{"clamps a negative lock on zero count", -1, 0, 0, 0},
		{"missing lock key", nil, 1, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(":6379")
			job1 := "job1"
			lockKey, lockInfoKey := redisKeyJobsLock(ns, job1), redisKeyJobsLockInfo(ns, job1)
			cleanKeyspace(ns, pool)

			seedRedis(t, pool, cmd("HSET", lockInfoKey, "gone", tc.orphan))
			if tc.lock != nil {
				seedRedis(t, pool, cmd("SET", lockKey, tc.lock))
			}
			if tc.live != 0 {
				seedRedis(t, pool,
					cmd("SADD", redisKeyWorkerPools(ns), "live"),
					cmd("HSET", redisKeyHeartbeat(ns, "live"), "heartbeat_at", time.Now().Unix()),
					cmd("HSET", lockInfoKey, "live", tc.live),
				)
			}

			reaper := newDeadPoolReaper(ns, pool, []string{job1})
			assert.NoError(t, reaper.reap())
			assert.NoError(t, reaper.reap())
			assert.EqualValues(t, tc.wantLock, lockCount(pool, lockKey))
			assertNoLockInfo(t, pool, lockInfoKey, "gone")
		})
	}
}

func TestDeadPoolReaperOrphanedLockPassesReplaced(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"
	job1 := "job1"
	lockKey, lockInfoKey := redisKeyJobsLock(ns, job1), redisKeyJobsLockInfo(ns, job1)
	cleanKeyspace(ns, pool)

	seedRedis(t, pool, cmd("SET", lockKey, 1), cmd("HSET", lockInfoKey, "p", 1))
	reaper := newDeadPoolReaper(ns, pool, []string{job1})
	assert.NoError(t, reaper.reap()) // orphan

	seedRedis(t, pool,
		cmd("SADD", redisKeyWorkerPools(ns), "p"),
		cmd("HSET", redisKeyHeartbeat(ns, "p"), "heartbeat_at", time.Now().Unix()),
	)
	assert.NoError(t, reaper.reap()) // live

	seedRedis(t, pool, cmd("SREM", redisKeyWorkerPools(ns), "p"), cmd("DEL", redisKeyHeartbeat(ns, "p")))
	assert.NoError(t, reaper.reap()) // orphan again, first pass
	assertLockInfo(t, pool, lockInfoKey, "p", 1)

	assert.NoError(t, reaper.reap())
	assertNoLockInfo(t, pool, lockInfoKey, "p")
	assert.EqualValues(t, 0, lockCount(pool, lockKey))
}

func TestDeadPoolReaperOrphanedLockTwoReapers(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"
	job1 := "job1"
	lockKey, lockInfoKey := redisKeyJobsLock(ns, job1), redisKeyJobsLockInfo(ns, job1)
	cleanKeyspace(ns, pool)

	seedRedis(t, pool, cmd("SET", lockKey, 2), cmd("HSET", lockInfoKey, "gone", 1))
	r1 := newDeadPoolReaper(ns, pool, []string{job1})
	r2 := newDeadPoolReaper(ns, pool, []string{job1})
	for i := 0; i < 2; i++ {
		assert.NoError(t, r1.reap())
		assert.NoError(t, r2.reap())
	}
	assert.EqualValues(t, 1, lockCount(pool, lockKey))
	assertNoLockInfo(t, pool, lockInfoKey, "gone")
}

func TestDeadPoolReaperOrphanedLockScriptErrorContinues(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"
	job1, job2 := "job1", "job2"
	cleanKeyspace(ns, pool)

	seedRedis(t, pool,
		cmd("SADD", redisKeyWorkerPools(ns), "live"),
		cmd("HSET", redisKeyHeartbeat(ns, "live"), "heartbeat_at", time.Now().Unix()),
		cmd("HSET", redisKeyJobsLockInfo(ns, job1), "live", "abc"),
		cmd("SET", redisKeyJobsLock(ns, job1), 1),
		cmd("HSET", redisKeyJobsLockInfo(ns, job1), "gone", 1),
		cmd("SET", redisKeyJobsLock(ns, job2), 1),
		cmd("HSET", redisKeyJobsLockInfo(ns, job2), "gone", 1),
	)

	reaper := newDeadPoolReaper(ns, pool, []string{job1, job2})
	assert.NoError(t, reaper.reap())
	assert.NoError(t, reaper.reap())
	assert.EqualValues(t, 1, lockCount(pool, redisKeyJobsLock(ns, job1)))
	assertLockInfo(t, pool, redisKeyJobsLockInfo(ns, job1), "gone", 1)
	assert.EqualValues(t, 0, lockCount(pool, redisKeyJobsLock(ns, job2)))
	assertNoLockInfo(t, pool, redisKeyJobsLockInfo(ns, job2), "gone")
}

func TestDeadPoolReaperOrphanedLockAfterDeadPool(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"
	job1, job2 := "job1", "job2"
	cleanKeyspace(ns, pool)

	// a dead pool in worker_pools whose heartbeat names only job1
	seedRedis(t, pool,
		cmd("SADD", redisKeyWorkerPools(ns), "dead"),
		cmd("HSET", redisKeyHeartbeat(ns, "dead"), "heartbeat_at", time.Now().Add(-time.Hour).Unix(), "job_names", job1),
		cmd("SET", redisKeyJobsLock(ns, job1), 2),
		cmd("HSET", redisKeyJobsLockInfo(ns, job1), "dead", 1),
		cmd("SET", redisKeyJobsLock(ns, job2), 1),
		cmd("HSET", redisKeyJobsLockInfo(ns, job2), "dead", 1),
	)

	reaper := newDeadPoolReaper(ns, pool, []string{job1, job2})
	assert.NoError(t, reaper.reap())
	assert.EqualValues(t, 1, lockCount(pool, redisKeyJobsLock(ns, job1)))
	assert.EqualValues(t, 1, lockCount(pool, redisKeyJobsLock(ns, job2)))

	assert.NoError(t, reaper.reap())
	assert.EqualValues(t, 1, lockCount(pool, redisKeyJobsLock(ns, job1)))
	assert.EqualValues(t, 0, lockCount(pool, redisKeyJobsLock(ns, job2)))
	assertNoLockInfo(t, pool, redisKeyJobsLockInfo(ns, job2), "dead")
}

func TestDeadPoolReaperOrphanedLockReadErrors(t *testing.T) {
	ns := "work"
	job1, job2 := "job1", "job2"

	t.Run("heartbeat read error", func(t *testing.T) {
		pool := newTestPool(":6379")
		cleanKeyspace(ns, pool)
		seedRedis(t, pool,
			cmd("SET", redisKeyHeartbeat(ns, "p"), "not a hash"),
			cmd("SET", redisKeyJobsLock(ns, job1), 1),
			cmd("HSET", redisKeyJobsLockInfo(ns, job1), "p", 1),
		)
		reaper := newDeadPoolReaper(ns, pool, []string{job1})
		assert.NoError(t, reaper.reap())
		assert.NotContains(t, reaper.prevOrphans, "p")
		assert.NoError(t, reaper.reap())
		assert.EqualValues(t, 1, lockCount(pool, redisKeyJobsLock(ns, job1)))
		assertLockInfo(t, pool, redisKeyJobsLockInfo(ns, job1), "p", 1)
	})

	t.Run("worker_pools read error", func(t *testing.T) {
		pool := newTestPool(":6379")
		cleanKeyspace(ns, pool)
		seedRedis(t, pool,
			cmd("SET", redisKeyWorkerPools(ns), "not a set"),
			cmd("SET", redisKeyJobsLock(ns, job1), 1),
			cmd("HSET", redisKeyJobsLockInfo(ns, job1), "gone", 1),
		)
		reaper := newDeadPoolReaper(ns, pool, []string{job1})
		assert.Error(t, reaper.reap())
		assert.Empty(t, reaper.prevOrphans)
		assert.Error(t, reaper.reap())
		assert.EqualValues(t, 1, lockCount(pool, redisKeyJobsLock(ns, job1)))
		assertLockInfo(t, pool, redisKeyJobsLockInfo(ns, job1), "gone", 1)
	})

	t.Run("worker_pools read error keeps the previous pass", func(t *testing.T) {
		pool := newTestPool(":6379")
		cleanKeyspace(ns, pool)
		seedRedis(t, pool,
			cmd("SET", redisKeyJobsLock(ns, job1), 1),
			cmd("HSET", redisKeyJobsLockInfo(ns, job1), "gone", 1),
		)
		reaper := newDeadPoolReaper(ns, pool, []string{job1})
		assert.NoError(t, reaper.reap())
		seedRedis(t, pool, cmd("SET", redisKeyWorkerPools(ns), "not a set"))
		assert.Error(t, reaper.reap())
		assertLockInfo(t, pool, redisKeyJobsLockInfo(ns, job1), "gone", 1)
		seedRedis(t, pool, cmd("DEL", redisKeyWorkerPools(ns)))
		assert.NoError(t, reaper.reap())
		assert.EqualValues(t, 0, lockCount(pool, redisKeyJobsLock(ns, job1)))
		assertNoLockInfo(t, pool, redisKeyJobsLockInfo(ns, job1), "gone")
	})

	t.Run("dead pool loop error", func(t *testing.T) {
		pool := newTestPool(":6379")
		cleanKeyspace(ns, pool)
		seedRedis(t, pool,
			cmd("SADD", redisKeyWorkerPools(ns), "broken"),
			cmd("SET", redisKeyHeartbeat(ns, "broken"), "not a hash"),
			cmd("SET", redisKeyJobsLock(ns, job1), 1),
			cmd("HSET", redisKeyJobsLockInfo(ns, job1), "gone", 1),
		)
		reaper := newDeadPoolReaper(ns, pool, []string{job1})
		assert.Error(t, reaper.reap())
		assert.Error(t, reaper.reap())
		assert.EqualValues(t, 0, lockCount(pool, redisKeyJobsLock(ns, job1)))
		assertNoLockInfo(t, pool, redisKeyJobsLockInfo(ns, job1), "gone")
	})

	t.Run("lock info read error on one job type", func(t *testing.T) {
		pool := newTestPool(":6379")
		cleanKeyspace(ns, pool)
		seedRedis(t, pool,
			cmd("SET", redisKeyJobsLockInfo(ns, job1), "not a hash"),
			cmd("SET", redisKeyJobsLock(ns, job2), 1),
			cmd("HSET", redisKeyJobsLockInfo(ns, job2), "gone", 1),
		)
		reaper := newDeadPoolReaper(ns, pool, []string{job1, job2})
		assert.NoError(t, reaper.reap())
		assert.NoError(t, reaper.reap())
		assert.EqualValues(t, 0, lockCount(pool, redisKeyJobsLock(ns, job2)))
		assertNoLockInfo(t, pool, redisKeyJobsLockInfo(ns, job2), "gone")
	})
}

func TestReapOrphanedLockScript(t *testing.T) {
	ns := "work"
	job1 := "job1"
	lockKey, lockInfoKey := redisKeyJobsLock(ns, job1), redisKeyJobsLockInfo(ns, job1)
	cases := []struct {
		name        string
		seed        [][]interface{}
		wantErr     bool
		wantRemoved int64
		wantLock    string
	}{
		{"in worker_pools", [][]interface{}{
			cmd("SADD", redisKeyWorkerPools(ns), "p"),
		}, false, 0, "1"},
		{"fresh heartbeat", [][]interface{}{
			cmd("HSET", redisKeyHeartbeat(ns, "p"), "heartbeat_at", time.Now().Unix()),
		}, false, 0, "1"},
		{"heartbeat without heartbeat_at", [][]interface{}{
			cmd("HSET", redisKeyHeartbeat(ns, "p"), "job_names", job1),
		}, false, 1, "0"},
		{"non-numeric lock", [][]interface{}{
			cmd("SET", lockKey, "abc"),
		}, true, 0, "abc"},
		{"non-numeric lock info", [][]interface{}{
			cmd("SADD", redisKeyWorkerPools(ns), "live"),
			cmd("HSET", lockInfoKey, "live", "abc"),
		}, true, 0, "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(":6379")
			cleanKeyspace(ns, pool)
			seedRedis(t, pool, cmd("SET", lockKey, 1), cmd("HSET", lockInfoKey, "p", 1))
			seedRedis(t, pool, tc.seed...)

			reaper := newDeadPoolReaper(ns, pool, []string{job1})
			conn := pool.Get()
			defer conn.Close()
			removed, err := redis.Int64(reaper.orphanedLockScript.Do(conn, lockKey, lockInfoKey,
				redisKeyWorkerPools(ns), redisKeyHeartbeat(ns, "p"), "p", time.Now().Add(-reaper.deadTime).Unix()))
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.wantRemoved, removed)
			}

			lock, err := redis.String(conn.Do("GET", lockKey))
			assert.NoError(t, err)
			assert.Equal(t, tc.wantLock, lock)
			if tc.wantRemoved == 0 {
				assertLockInfo(t, pool, lockInfoKey, "p", 1)
			}
		})
	}
}

func TestDeadPoolReaperDeadPoolPayback(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"
	job1 := "job1"
	cleanKeyspace(ns, pool)

	seedRedis(t, pool,
		cmd("SADD", redisKeyWorkerPools(ns), "live", "dead"),
		cmd("HSET", redisKeyHeartbeat(ns, "live"), "heartbeat_at", time.Now().Unix()),
		cmd("HSET", redisKeyHeartbeat(ns, "dead"), "heartbeat_at", time.Now().Add(-time.Hour).Unix(), "job_names", job1),
		cmd("SET", redisKeyJobsLock(ns, job1), 2),
		cmd("HSET", redisKeyJobsLockInfo(ns, job1), "live", 1, "dead", 1),
		// leftover entries, e.g. from unique jobs, each requeued with its own decrement
		cmd("LPUSH", redisKeyJobsInProgress(ns, "dead", job1), "a", "b", "c"),
	)

	reaper := newDeadPoolReaper(ns, pool, []string{job1})
	assert.NoError(t, reaper.reap())
	assert.EqualValues(t, 1, lockCount(pool, redisKeyJobsLock(ns, job1)))
	assert.EqualValues(t, 3, listSize(pool, redisKeyJobs(ns, job1)))
}

func cmd(args ...interface{}) []interface{} {
	return args
}

func seedRedis(t *testing.T, pool *redis.Pool, cmds ...[]interface{}) {
	conn := pool.Get()
	defer conn.Close()

	for _, c := range cmds {
		_, err := conn.Do(c[0].(string), c[1:]...)
		assert.NoError(t, err)
	}
}

// lockCount reads a lock that may not exist; getInt64 panics on a missing key
func lockCount(pool *redis.Pool, key string) int64 {
	conn := pool.Get()
	defer conn.Close()

	v, err := redis.Int64(conn.Do("GET", key))
	if err == redis.ErrNil {
		return 0
	}
	if err != nil {
		panic("could not GET lock: " + err.Error())
	}
	return v
}

func assertLockInfo(t *testing.T, pool *redis.Pool, key, poolID string, want int64) {
	assert.Equal(t, want, hgetInt64(pool, key, poolID))
}

func assertNoLockInfo(t *testing.T, pool *redis.Pool, key, poolID string) {
	conn := pool.Get()
	defer conn.Close()

	exists, err := redis.Bool(conn.Do("HEXISTS", key, poolID))
	assert.NoError(t, err)
	assert.False(t, exists)
}
