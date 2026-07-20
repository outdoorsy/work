package work

import (
	"fmt"
	"testing"

	"github.com/gomodule/redigo/redis"
	"github.com/stretchr/testify/assert"
)

func TestObserverStarted(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"

	tMock := int64(1425263401)
	setNowEpochSecondsMock(tMock)
	defer resetNowEpochSecondsMock()

	observer := newObserver(ns, pool, "abcd")
	observer.start()
	observer.observeStarted("foo", "bar", Q{"a": 1, "b": "wat"})
	//observer.observeDone("foo", "bar", nil)
	observer.drain()
	observer.stop()

	h := readHash(pool, redisKeyWorkerObservation(ns, "abcd"))
	assert.Equal(t, "foo", h["job_name"])
	assert.Equal(t, "bar", h["job_id"])
	assert.Equal(t, fmt.Sprint(tMock), h["started_at"])
	assert.Equal(t, `{"a":1,"b":"wat"}`, h["args"])
}

func TestObserverStartedDone(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"

	tMock := int64(1425263401)
	setNowEpochSecondsMock(tMock)
	defer resetNowEpochSecondsMock()

	observer := newObserver(ns, pool, "abcd")
	observer.start()
	observer.observeStarted("foo", "bar", Q{"a": 1, "b": "wat"})
	observer.observeDone("foo", "bar", nil)
	observer.drain()
	observer.stop()

	h := readHash(pool, redisKeyWorkerObservation(ns, "abcd"))
	assert.Equal(t, 0, len(h))
}

func TestObserverCheckin(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"

	observer := newObserver(ns, pool, "abcd")
	observer.start()

	tMock := int64(1425263401)
	setNowEpochSecondsMock(tMock)
	defer resetNowEpochSecondsMock()
	observer.observeStarted("foo", "bar", Q{"a": 1, "b": "wat"})

	tMockCheckin := int64(1425263402)
	setNowEpochSecondsMock(tMockCheckin)
	observer.observeCheckin("foo", "bar", "doin it")
	observer.drain()
	observer.stop()

	h := readHash(pool, redisKeyWorkerObservation(ns, "abcd"))
	assert.Equal(t, "foo", h["job_name"])
	assert.Equal(t, "bar", h["job_id"])
	assert.Equal(t, fmt.Sprint(tMock), h["started_at"])
	assert.Equal(t, `{"a":1,"b":"wat"}`, h["args"])
	assert.Equal(t, "doin it", h["checkin"])
	assert.Equal(t, fmt.Sprint(tMockCheckin), h["checkin_at"])
}

func TestObserverCheckinFromJob(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"

	observer := newObserver(ns, pool, "abcd")
	observer.start()

	tMock := int64(1425263401)
	setNowEpochSecondsMock(tMock)
	defer resetNowEpochSecondsMock()
	observer.observeStarted("foo", "barbar", Q{"a": 1, "b": "wat"})

	tMockCheckin := int64(1425263402)
	setNowEpochSecondsMock(tMockCheckin)

	j := &Job{Name: "foo", ID: "barbar", observer: observer}
	j.Checkin("sup")

	observer.drain()
	observer.stop()

	h := readHash(pool, redisKeyWorkerObservation(ns, "abcd"))
	assert.Equal(t, "foo", h["job_name"])
	assert.Equal(t, "barbar", h["job_id"])
	assert.Equal(t, fmt.Sprint(tMock), h["started_at"])
	assert.Equal(t, "sup", h["checkin"])
	assert.Equal(t, fmt.Sprint(tMockCheckin), h["checkin_at"])
}

// TestObserverDoesNotShareArgumentsMapWithCaller guards against the
// production crash this fix addresses: "fatal error: concurrent map
// iteration and map write" in writeStatus's json.Marshal, caused by the
// observer aliasing the caller's arguments map instead of copying it. Run
// with -race: without the fix, mutating the original map concurrently with
// draining (which marshals the stored observation) is flagged as a data
// race; with the fix, the observer only ever touches its own copy.
func TestObserverDoesNotShareArgumentsMapWithCaller(t *testing.T) {
	pool := newTestPool(":6379")
	ns := "work"

	observer := newObserver(ns, pool, "race-test")
	observer.start()
	defer observer.stop()

	args := map[string]interface{}{"a": 1}
	observer.observeStarted("foo", "bar", args)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 10000 {
			args[fmt.Sprintf("k%d", i%50)] = i
		}
	}()

	for range 200 {
		observer.drain()
	}

	<-done
}

// TestCopyArgsNil confirms copyArgs(nil) returns nil rather than an empty
// map, and that observeStarted with nil args doesn't panic and round-trips
// through writeStatus the same way the old aliasing code did (writeStatus's
// len(obv.arguments) == 0 check treats nil and empty identically, so this
// change must not alter that).
func TestCopyArgsNil(t *testing.T) {
	assert.Nil(t, copyArgs(nil))

	pool := newTestPool(":6379")
	ns := "work"

	tMock := int64(1425263401)
	setNowEpochSecondsMock(tMock)
	defer resetNowEpochSecondsMock()

	observer := newObserver(ns, pool, "nil-args-test")
	observer.start()
	observer.observeStarted("foo", "bar", nil)
	observer.drain()
	observer.stop()

	h := readHash(pool, redisKeyWorkerObservation(ns, "nil-args-test"))
	assert.Equal(t, "foo", h["job_name"])
	assert.Equal(t, "bar", h["job_id"])
	assert.Equal(t, "", h["args"])
}

// TestCopyArgsIsShallowNotDeep documents, with an actual assertion rather
// than just a comment, exactly what copyArgs does and doesn't protect
// against: it's a *different map* from the input (top-level keys are
// independent -- this is the whole point of the fix), but a nested
// reference value (a slice or map stored as one of the values) is the
// *same* underlying object in both copies. If this ever needs to change to
// a deep copy, this test is the one that should start failing and get
// updated deliberately, rather than the distinction silently drifting.
func TestCopyArgsIsShallowNotDeep(t *testing.T) {
	nested := map[string]interface{}{"x": 1}
	original := map[string]interface{}{"top": "a", "nested": nested}

	cp := copyArgs(original)

	// Top-level independence: this is what the fix actually guarantees.
	cp["top"] = "b"
	assert.Equal(t, "a", original["top"], "mutating the copy's top-level key must not affect the original")

	original["new-top-key"] = "c"
	_, ok := cp["new-top-key"]
	assert.False(t, ok, "adding a top-level key to the original must not affect the copy")

	// Nested sharing: this is the documented, accepted limitation, not an
	// oversight -- assert it explicitly so a future deep-copy change is a
	// deliberate, visible diff here.
	nested["x"] = 2
	assert.Equal(t, 2, cp["nested"].(map[string]interface{})["x"], "nested values are shared by design (shallow copy)")
}

func readHash(pool *redis.Pool, key string) map[string]string {
	m := make(map[string]string)

	conn := pool.Get()
	defer conn.Close()

	v, err := redis.Strings(conn.Do("HGETALL", key))
	if err != nil {
		panic("could not delete retry/dead queue: " + err.Error())
	}

	for i, l := 0, len(v); i < l; i += 2 {
		m[v[i]] = v[i+1]
	}

	return m
}
