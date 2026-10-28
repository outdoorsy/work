package work

import (
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/gomodule/redigo/redis"
)

const (
	deadTime          = 10 * time.Second // 2 x heartbeat
	reapPeriod        = 10 * time.Minute
	reapJitterSecs    = 30
	requeueKeysPerJob = 4
)

type deadPoolReaper struct {
	namespace   string
	pool        *redis.Pool
	deadTime    time.Duration
	reapPeriod  time.Duration
	curJobTypes []string

	stopChan         chan struct{}
	doneStoppingChan chan struct{}

	// pool ids that were orphans in the previous pass; a pool is reaped only after two passes
	prevOrphans        map[string]struct{}
	orphanedLockScript *redis.Script
}

func newDeadPoolReaper(namespace string, pool *redis.Pool, curJobTypes []string) *deadPoolReaper {
	return &deadPoolReaper{
		namespace:        namespace,
		pool:             pool,
		deadTime:         deadTime,
		reapPeriod:       reapPeriod,
		curJobTypes:      curJobTypes,
		stopChan:         make(chan struct{}),
		doneStoppingChan: make(chan struct{}),

		prevOrphans:        map[string]struct{}{},
		orphanedLockScript: redis.NewScript(4, redisLuaReapOrphanedLock),
	}
}

func (r *deadPoolReaper) start() {
	go r.loop()
}

func (r *deadPoolReaper) stop() {
	r.stopChan <- struct{}{}
	<-r.doneStoppingChan
}

func (r *deadPoolReaper) loop() {
	// Reap immediately after we provide some time for initialization
	timer := time.NewTimer(r.deadTime)
	defer timer.Stop()

	for {
		select {
		case <-r.stopChan:
			r.doneStoppingChan <- struct{}{}
			return
		case <-timer.C:
			// Schedule next occurrence periodically with jitter
			timer.Reset(r.reapPeriod + time.Duration(rand.Intn(reapJitterSecs))*time.Second)

			// Reap
			if err := r.reap(); err != nil {
				logError("dead_pool_reaper.reap", err)
			}
		}
	}
}

func (r *deadPoolReaper) reap() error {
	err := r.reapDeadPools()
	r.reapOrphanedLocks()
	return err
}

func (r *deadPoolReaper) reapDeadPools() error {
	// Get dead pools
	deadPoolIDs, err := r.findDeadPools()
	if err != nil {
		return err
	}

	conn := r.pool.Get()
	defer conn.Close()

	workerPoolsKey := redisKeyWorkerPools(r.namespace)

	// Cleanup all dead pools
	for deadPoolID, jobTypes := range deadPoolIDs {
		lockJobTypes := jobTypes
		// if we found jobs from the heartbeat, requeue them and remove the heartbeat
		if len(jobTypes) > 0 {
			r.requeueInProgressJobs(deadPoolID, jobTypes)
			if _, err = conn.Do("DEL", redisKeyHeartbeat(r.namespace, deadPoolID)); err != nil {
				return err
			}
		} else {
			// try to clean up locks for the current set of jobs if heartbeat was not found
			lockJobTypes = r.curJobTypes
		}
		// Remove dead pool from worker pools set
		if _, err = conn.Do("SREM", workerPoolsKey, deadPoolID); err != nil {
			return err
		}
		// Cleanup any stale lock info
		if err = r.cleanStaleLockInfo(deadPoolID, lockJobTypes); err != nil {
			return err
		}
	}

	return nil
}

// reapOrphanedLocks frees locks still held by pools that left worker_pools, e.g. a pool whose lock
// release failed before a graceful stop. findDeadPools only walks worker_pools, so it never sees them.
func (r *deadPoolReaper) reapOrphanedLocks() {
	conn := r.pool.Get()
	defer conn.Close()

	workerPoolsKey := redisKeyWorkerPools(r.namespace)
	workerPoolIDs, err := redis.Strings(conn.Do("SMEMBERS", workerPoolsKey))
	if err != nil {
		logError("dead_pool_reaper.orphan.worker_pools", err)
		return
	}
	livePools := make(map[string]bool, len(workerPoolIDs))
	for _, poolID := range workerPoolIDs {
		livePools[poolID] = true
	}

	for _, jobType := range r.curJobTypes {
		conn.Send("HKEYS", redisKeyJobsLockInfo(r.namespace, jobType))
	}
	if err := conn.Flush(); err != nil {
		logError("dead_pool_reaper.orphan.lock_info", err)
		return
	}
	candidatesByJob := make(map[string][]string)
	var candidates []string
	seen := map[string]bool{}
	for _, jobType := range r.curJobTypes {
		poolIDs, err := redis.Strings(conn.Receive())
		if err != nil {
			logError("dead_pool_reaper.orphan.lock_info", err)
			continue
		}
		for _, poolID := range poolIDs {
			if livePools[poolID] {
				continue
			}
			candidatesByJob[jobType] = append(candidatesByJob[jobType], poolID)
			if !seen[poolID] {
				seen[poolID] = true
				candidates = append(candidates, poolID)
			}
		}
	}

	// compared as findDeadPools does: a heartbeat after the cutoff is fresh
	cutoff := time.Now().Add(-r.deadTime)
	for _, poolID := range candidates {
		conn.Send("HGET", redisKeyHeartbeat(r.namespace, poolID), "heartbeat_at")
	}
	if err := conn.Flush(); err != nil {
		logError("dead_pool_reaper.orphan.heartbeat", err)
		return
	}
	orphans := make(map[string]struct{}, len(candidates))
	for _, poolID := range candidates {
		heartbeatAt, err := redis.Int64(conn.Receive())
		if err == redis.ErrNil || (err == nil && !time.Unix(heartbeatAt, 0).After(cutoff)) {
			orphans[poolID] = struct{}{}
		} else if err != nil {
			logError("dead_pool_reaper.orphan.heartbeat", err)
		}
	}

	confirmed := r.prevOrphans
	r.prevOrphans = orphans

	if err := r.orphanedLockScript.Load(conn); err != nil {
		logError("dead_pool_reaper.orphan.load", err)
		return
	}
	reaped := 0
	for _, jobType := range r.curJobTypes {
		lockKey := redisKeyJobsLock(r.namespace, jobType)
		lockInfoKey := redisKeyJobsLockInfo(r.namespace, jobType)
		sent := 0
		for _, poolID := range candidatesByJob[jobType] {
			_, orphan := orphans[poolID]
			_, wasOrphan := confirmed[poolID]
			if !orphan || !wasOrphan {
				continue
			}
			r.orphanedLockScript.SendHash(conn, lockKey, lockInfoKey, workerPoolsKey, redisKeyHeartbeat(r.namespace, poolID), poolID, cutoff.Unix())
			sent++
		}
		if sent == 0 {
			continue
		}
		if err := conn.Flush(); err != nil {
			logError("dead_pool_reaper.orphan", err)
			continue
		}
		for i := 0; i < sent; i++ {
			removed, err := redis.Int(conn.Receive())
			if err != nil {
				logError("dead_pool_reaper.orphan", err)
				continue
			}
			reaped += removed
		}
	}
	if reaped > 0 {
		fmt.Printf("INFO: dead_pool_reaper.orphan - reaped %d lock info entries of pools gone from worker_pools\n", reaped)
	}
}

func (r *deadPoolReaper) cleanStaleLockInfo(poolID string, jobTypes []string) error {
	numKeys := len(jobTypes) * 2
	redisReapLocksScript := redis.NewScript(numKeys, redisLuaReapStaleLocks)
	var scriptArgs = make([]interface{}, 0, numKeys+1) // +1 for argv[1]

	for _, jobType := range jobTypes {
		scriptArgs = append(scriptArgs, redisKeyJobsLock(r.namespace, jobType), redisKeyJobsLockInfo(r.namespace, jobType))
	}
	scriptArgs = append(scriptArgs, poolID) // ARGV[1]

	conn := r.pool.Get()
	defer conn.Close()
	if _, err := redisReapLocksScript.Do(conn, scriptArgs...); err != nil {
		return err
	}

	return nil
}

func (r *deadPoolReaper) requeueInProgressJobs(poolID string, jobTypes []string) error {
	numKeys := len(jobTypes) * requeueKeysPerJob
	redisRequeueScript := redis.NewScript(numKeys, redisLuaReenqueueJob)
	var scriptArgs = make([]interface{}, 0, numKeys+1)

	for _, jobType := range jobTypes {
		// pops from in progress, push into job queue and decrement the queue lock
		scriptArgs = append(scriptArgs, redisKeyJobsInProgress(r.namespace, poolID, jobType), redisKeyJobs(r.namespace, jobType), redisKeyJobsLock(r.namespace, jobType), redisKeyJobsLockInfo(r.namespace, jobType)) // KEYS[1-4 * N]
	}
	scriptArgs = append(scriptArgs, poolID) // ARGV[1]

	conn := r.pool.Get()
	defer conn.Close()

	// Keep moving jobs until all queues are empty
	for {
		values, err := redis.Values(redisRequeueScript.Do(conn, scriptArgs...))
		if err == redis.ErrNil {
			return nil
		} else if err != nil {
			return err
		}

		if len(values) != 3 {
			return fmt.Errorf("need 3 elements back")
		}
	}
}

func (r *deadPoolReaper) findDeadPools() (map[string][]string, error) {
	conn := r.pool.Get()
	defer conn.Close()

	workerPoolsKey := redisKeyWorkerPools(r.namespace)

	workerPoolIDs, err := redis.Strings(conn.Do("SMEMBERS", workerPoolsKey))
	if err != nil {
		return nil, err
	}

	deadPools := map[string][]string{}
	for _, workerPoolID := range workerPoolIDs {
		heartbeatKey := redisKeyHeartbeat(r.namespace, workerPoolID)
		heartbeatAt, err := redis.Int64(conn.Do("HGET", heartbeatKey, "heartbeat_at"))
		if err == redis.ErrNil {
			// heartbeat expired, save dead pool and use cur set of jobs from reaper
			deadPools[workerPoolID] = []string{}
			continue
		}
		if err != nil {
			return nil, err
		}

		// Check that last heartbeat was long enough ago to consider the pool dead
		if time.Unix(heartbeatAt, 0).Add(r.deadTime).After(time.Now()) {
			continue
		}

		jobTypesList, err := redis.String(conn.Do("HGET", heartbeatKey, "job_names"))
		if err == redis.ErrNil {
			continue
		}
		if err != nil {
			return nil, err
		}

		deadPools[workerPoolID] = strings.Split(jobTypesList, ",")
	}

	return deadPools, nil
}
