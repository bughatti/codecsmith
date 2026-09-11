// Package job defines the job row and its state machine.
package job

import "time"

// Status is the job state.
//
//	queued ──▶ processing ──▶ running ──▶ completed
//	   ▲                         │   ├──▶ skipped   (output would not be smaller)
//	   │                         │   ├──▶ failed
//	   │                         │   └──▶ cancelled
//	   └──────── interrupted ◀───┘        (worker died mid-job; re-claimed first)
type Status string

const (
	StatusQueued      Status = "queued"
	StatusProcessing  Status = "processing"
	StatusRunning     Status = "running"
	StatusCompleted   Status = "completed"
	StatusFailed      Status = "failed"
	StatusInterrupted Status = "interrupted"
	StatusSkipped     Status = "skipped"
	StatusCancelled   Status = "cancelled"
)

// Active statuses are those a worker is (or should be) working on.
var Active = []Status{StatusProcessing, StatusRunning}

// Pending statuses are claimable.
var Pending = []Status{StatusQueued, StatusInterrupted}

// Terminal statuses will not change without user action.
var Terminal = []Status{StatusCompleted, StatusFailed, StatusSkipped, StatusCancelled}

// Job is the row shape of the jobs table.
type Job struct {
	ID              int64
	FilePath        string
	Library         string
	Profile         string
	Status          Status
	Priority        int
	OriginalSize    int64
	NewSize         int64
	OriginalCodec   string
	TargetCodec     string
	CreatedAt       time.Time
	StartedAt       *time.Time
	CompletedAt     *time.Time
	UpdatedAt       *time.Time
	ErrorMessage    string
	Progress        float64
	Speed           float64 // encode speed multiple of realtime
	FPS             float64
	ETASeconds      *int
	WorkerID        string
	CancelRequested bool
}

// Stats is an aggregate over jobs.
type Stats struct {
	Total      int64
	Completed  int64
	Failed     int64
	Skipped    int64
	Cancelled  int64
	Queued     int64
	Running    int64
	TotalSaved int64
}
