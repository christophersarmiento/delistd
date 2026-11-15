package web

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"sync"
	"time"
)

type JobStatus string

const (
	JobStatusRunning   JobStatus = "running"
	JobStatusCompleted JobStatus = "completed"
	JobStatusCancelled JobStatus = "cancelled"
	JobStatusPaused    JobStatus = "paused" // the daily limit was reached
	JobStatusError     JobStatus = "error"  // stopped on an auth or config error
)

// maxAuthFailures in a row stop a send: the account or password is wrong and
// every further attempt would only add to the provider's lockout counter.
const maxAuthFailures = 3

// Job is one background send over a list of brokers. Everything but the
// immutable fields is guarded by mu; other code goes through the methods, and
// JSON encoding takes a consistent snapshot.
type Job struct {
	ID              string    `json:"id"`
	ProfileID       string    `json:"profile_id"`
	Status          JobStatus `json:"status"`
	Progress        int       `json:"progress"`
	Sent            int       `json:"sent"`
	Failed          int       `json:"failed"`
	Total           int       `json:"total"`
	CurrentBroker   string    `json:"current_broker"`
	CurrentBrokerID string    `json:"current_broker_id"`
	StartedAt       time.Time `json:"started_at"`
	CompletedAt     time.Time `json:"completed_at,omitempty"`
	Error           string    `json:"error,omitempty"`
	ErrorType       string    `json:"error_type,omitempty"`  // "auth", "rate_limit", ...
	DailyLimit      int       `json:"daily_limit,omitempty"` // emails allowed per day
	DaySent         int       `json:"day_sent,omitempty"`    // emails sent today

	ctx        context.Context
	cancelFunc context.CancelFunc
	mu         sync.Mutex
	authFails  int // consecutive
}

// do runs change with the job locked.
func (j *Job) do(change func()) {
	j.mu.Lock()
	defer j.mu.Unlock()
	change()
}

func (j *Job) idle() { j.CurrentBroker, j.CurrentBrokerID = "", "" }

// Update records progress. The broker ID lets the brokers page refresh just
// the row that changed instead of the whole list on every poll.
func (j *Job) Update(sent, failed int, currentBroker, currentBrokerID string) {
	j.do(func() {
		j.Sent, j.Failed = sent, failed
		j.CurrentBroker, j.CurrentBrokerID = currentBroker, currentBrokerID
		if j.Total > 0 {
			j.Progress = (sent + failed) * 100 / j.Total
		}
	})
}

func (j *Job) Complete() {
	j.do(func() {
		j.Status, j.CompletedAt, j.Progress = JobStatusCompleted, time.Now(), 100
		j.idle()
	})
}

// StopWithError ends the job early and keeps why. The status stays
// "completed"; the page tells the two apart by ErrorType.
func (j *Job) StopWithError(errorType, message string) {
	j.do(func() {
		j.Status, j.CompletedAt = JobStatusCompleted, time.Now()
		j.Error, j.ErrorType = message, errorType
		j.idle()
	})
}

// Pause marks the job as stopped by the daily limit.
func (j *Job) Pause(daySent int, message string) {
	j.do(func() { j.DaySent, j.Status, j.Error = daySent, JobStatusPaused, message })
}

// RecordAuthFailure counts one more login failure in a row and reports
// whether the job should stop.
func (j *Job) RecordAuthFailure() (stop bool) {
	j.do(func() {
		j.authFails++
		stop = j.authFails >= maxAuthFailures
	})
	return stop
}

func (j *Job) ResetAuthFailures() { j.do(func() { j.authFails = 0 }) }

func (j *Job) SetDailyLimit(limit int) { j.do(func() { j.DailyLimit = limit }) }

// Cancel stops a running job and cancels its context; any other status is
// left as it is.
func (j *Job) Cancel() {
	j.do(func() {
		if j.Status != JobStatusRunning {
			return
		}
		j.Status, j.CompletedAt = JobStatusCancelled, time.Now()
		if j.cancelFunc != nil {
			j.cancelFunc()
		}
	})
}

func (j *Job) GetStatus() (status JobStatus) {
	j.do(func() { status = j.Status })
	return status
}

func (j *Job) IsCancelled() bool { return j.GetStatus() == JobStatusCancelled }

// finishedBefore reports a job that is no longer running and ended before
// cutoff.
func (j *Job) finishedBefore(cutoff time.Time) (old bool) {
	j.do(func() { old = j.Status != JobStatusRunning && j.CompletedAt.Before(cutoff) })
	return old
}

func (j *Job) Context() context.Context { return j.ctx }

// MarshalJSON encodes a snapshot, so a status poll never sees a job halfway
// through an Update.
func (j *Job) MarshalJSON() ([]byte, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	type plain Job // without the method, so this does not recurse
	return json.Marshal((*plain)(j))
}

// jobRetention is how long a finished job stays in memory. Jobs are not
// stored anywhere else; a week is plenty to look back at a send.
const jobRetention = 7 * 24 * time.Hour

// JobManager keeps the jobs of one server process. Finished ones older than
// jobRetention are dropped whenever a new job is created, so there is no
// background goroutine to leak.
type JobManager struct {
	mu   sync.RWMutex
	jobs map[string]*Job
}

func NewJobManager() *JobManager { return &JobManager{jobs: map[string]*Job{}} }

// Create starts a job for profileID. It does not look for a running one: use
// CreateIfNoActive when only one job per profile may run.
func (jm *JobManager) Create(total int, profileID string) *Job {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	return jm.add(total, profileID)
}

// add inserts a new job; the caller holds jm.mu.
func (jm *JobManager) add(total int, profileID string) *Job {
	jm.evict(jobRetention)
	ctx, cancel := context.WithCancel(context.Background())
	job := &Job{ID: rand.Text(), ProfileID: profileID, Status: JobStatusRunning, Total: total, StartedAt: time.Now(), ctx: ctx, cancelFunc: cancel}
	jm.jobs[job.ID] = job
	return job
}

// CreateIfNoActive starts a job unless profileID already has one running,
// deciding under a single lock so two requests cannot both send. It returns
// the running job and false, or the new job and true.
func (jm *JobManager) CreateIfNoActive(total int, profileID string) (*Job, bool) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	if running := jm.running(profileID); running != nil {
		return running, false
	}
	return jm.add(total, profileID), true
}

// running returns a running job for profileID (any profile when empty); the
// caller holds jm.mu.
func (jm *JobManager) running(profileID string) *Job {
	for _, job := range jm.jobs {
		if job.GetStatus() == JobStatusRunning && (profileID == "" || job.ProfileID == profileID) {
			return job
		}
	}
	return nil
}

func (jm *JobManager) Get(id string) *Job {
	jm.mu.RLock()
	defer jm.mu.RUnlock()
	return jm.jobs[id]
}

// GetActive returns the running job of one profile, or nil. It is per profile
// so that two profiles can send at the same time, each within its own daily
// limit and history.
func (jm *JobManager) GetActive(profileID string) *Job {
	jm.mu.RLock()
	defer jm.mu.RUnlock()
	return jm.running(profileID)
}

func (jm *JobManager) AnyActive() bool {
	jm.mu.RLock()
	defer jm.mu.RUnlock()
	return jm.running("") != nil
}

// Cleanup drops finished jobs older than maxAge.
func (jm *JobManager) Cleanup(maxAge time.Duration) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	jm.evict(maxAge)
}

func (jm *JobManager) evict(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	for id, job := range jm.jobs {
		if job.finishedBefore(cutoff) {
			delete(jm.jobs, id)
		}
	}
}
