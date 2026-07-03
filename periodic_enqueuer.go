package work

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/robfig/cron"
)

const (
	periodicEnqueuerSleep   = 2 * time.Minute
	periodicEnqueuerHorizon = 4 * time.Minute
)

type periodicEnqueuer struct {
	namespace             string
	pool                  *redis.Pool
	periodicJobs          []*periodicJob
	scheduledPeriodicJobs []*scheduledPeriodicJob
	shouldEnqueueScript   *redis.Script
	stopChan              chan struct{}
	doneStoppingChan      chan struct{}
}

// redisLuaShouldEnqueue atomically checks whether the last periodic enqueue
// (KEYS[1]) happened more than ARGV[2] seconds before ARGV[1] (now), and if
// so, claims the window by setting KEYS[1] to ARGV[1] before returning 1.
// Doing the read, comparison, and write as one script closes the race where
// two processes both read a stale value and both decide they're the one
// that should enqueue.
var redisLuaShouldEnqueue = `
local last = redis.call('get', KEYS[1])
if (last == false) or (tonumber(last) < (tonumber(ARGV[1]) - tonumber(ARGV[2]))) then
	redis.call('set', KEYS[1], ARGV[1])
	return 1
end
return 0
`

type periodicJob struct {
	jobName  string
	spec     string
	schedule cron.Schedule
}

type scheduledPeriodicJob struct {
	scheduledAt      time.Time
	scheduledAtEpoch int64
	*periodicJob
}

func newPeriodicEnqueuer(namespace string, pool *redis.Pool, periodicJobs []*periodicJob) *periodicEnqueuer {
	return &periodicEnqueuer{
		namespace:           namespace,
		pool:                pool,
		periodicJobs:        periodicJobs,
		shouldEnqueueScript: redis.NewScript(1, redisLuaShouldEnqueue),
		stopChan:            make(chan struct{}),
		doneStoppingChan:    make(chan struct{}),
	}
}

func (pe *periodicEnqueuer) start() {
	go pe.loop()
}

func (pe *periodicEnqueuer) stop() {
	pe.stopChan <- struct{}{}
	<-pe.doneStoppingChan
}

func (pe *periodicEnqueuer) loop() {
	// Begin reaping periodically
	timer := time.NewTimer(periodicEnqueuerSleep + time.Duration(rand.Intn(30))*time.Second)
	defer timer.Stop()

	if pe.shouldEnqueue() {
		err := pe.enqueue()
		if err != nil {
			logError("periodic_enqueuer.loop.enqueue", err)
		}
	}

	for {
		select {
		case <-pe.stopChan:
			pe.doneStoppingChan <- struct{}{}
			return
		case <-timer.C:
			timer.Reset(periodicEnqueuerSleep + time.Duration(rand.Intn(30))*time.Second)
			if pe.shouldEnqueue() {
				err := pe.enqueue()
				if err != nil {
					logError("periodic_enqueuer.loop.enqueue", err)
				}
			}
		}
	}
}

func (pe *periodicEnqueuer) enqueue() error {
	now := nowEpochSeconds()
	nowTime := time.Unix(now, 0)
	horizon := nowTime.Add(periodicEnqueuerHorizon)

	conn := pe.pool.Get()
	defer conn.Close()

	for _, pj := range pe.periodicJobs {
		for t := pj.schedule.Next(nowTime); t.Before(horizon); t = pj.schedule.Next(t) {
			epoch := t.Unix()
			id := makeUniquePeriodicID(pj.jobName, pj.spec, epoch)

			job := &Job{
				Name: pj.jobName,
				ID:   id,

				// This is technically wrong, but this lets the bytes be identical for the same periodic job instance. If we don't do this, we'd need to use a different approach -- probably giving each periodic job its own history of the past 100 periodic jobs, and only scheduling a job if it's not in the history.
				EnqueuedAt: epoch,
				Args:       nil,
			}

			rawJSON, err := job.serialize()
			if err != nil {
				return err
			}

			_, err = conn.Do("ZADD", redisKeyScheduled(pe.namespace), epoch, rawJSON)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// shouldEnqueue atomically checks whether the periodic-enqueue window has
// elapsed and, if so, claims it for this process in the same Redis round
// trip. Only the single caller that wins the claim (across every worker
// pool process sharing this namespace) gets true back; every other process
// racing at the same tick gets false and skips enqueue() entirely.
//
// The previous implementation did a plain GET, branched on it in Go, and
// only SET the timestamp at the end of enqueue() -- a classic check-then-act
// race that let multiple processes both read a stale value and both decide
// they were the one that should enqueue. It also compared epoch seconds
// against periodicEnqueuerSleep/time.Minute (a bare "2"), which is off by a
// factor of 60: the intended 2-minute dedup window was actually about 2
// seconds, so the GET-based check almost never blocked anyone.
func (pe *periodicEnqueuer) shouldEnqueue() bool {
	conn := pe.pool.Get()
	defer conn.Close()

	thresholdSeconds := int64(periodicEnqueuerSleep / time.Second)
	claimed, err := redis.Int(pe.shouldEnqueueScript.Do(conn, redisKeyLastPeriodicEnqueue(pe.namespace), nowEpochSeconds(), thresholdSeconds))
	if err != nil {
		logError("periodic_enqueuer.should_enqueue", err)
		return true
	}

	return claimed == 1
}

func makeUniquePeriodicID(name, spec string, epoch int64) string {
	return fmt.Sprintf("periodic:%s:%s:%d", name, spec, epoch)
}
