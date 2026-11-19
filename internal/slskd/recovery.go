package slskd

// Boot-time recovery.
//
// slskd with flags.volatile: true drops every transfer record on restart, and
// its retention settings delete completed records after a while. A Provenance
// job must survive both, so recovery cross-checks three sources: the durable job
// record, slskd's live transfer list, and the bytes on disk.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultRecoveryGrace is how long a job may be invisible to slskd before it is
// declared interrupted. A batch that was just enqueued has no transfer records
// yet, and calling that interrupted would be a lie.
const DefaultRecoveryGrace = 2 * time.Minute

// Recovery reconciles durable jobs against slskd and the filesystem at startup.
type Recovery struct {
	Client *Client
	Store  *JobStore
	Mount  string
	Grace  time.Duration
	Now    func() time.Time
}

// RecoveryReport summarises one boot reconcile.
type RecoveryReport struct {
	// Checked is the number of pending jobs examined.
	Checked int
	// Active jobs still have transfers that slskd knows about.
	Active int
	// Recovered jobs are complete on disk but unknown to slskd (retention or a
	// volatile restart); they are ready to ingest.
	Recovered []string
	// Interrupted jobs exist in neither place and can be re-enqueued.
	Interrupted []string
	// LeftAlone jobs were too young to judge.
	LeftAlone []string
}

func (r *Recovery) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

// Run performs the reconcile.
//
// It refuses to mutate anything when slskd is unreachable: "cannot see slskd" is
// not "slskd has forgotten the job", and marking jobs interrupted on a network
// blip would produce a false history that a user then acts on.
func (r *Recovery) Run(ctx context.Context) (RecoveryReport, error) {
	report := RecoveryReport{}
	if r.Client == nil || r.Store == nil {
		return report, errors.New("recovery requires a client and a job store")
	}
	transfers, err := r.Client.ListDownloads(ctx, true)
	if err != nil {
		return report, fmt.Errorf("recovery needs the transfer list; not touching any job: %w", err)
	}
	pending, err := r.Store.Pending()
	if err != nil {
		return report, err
	}
	grace := r.Grace
	if grace == 0 {
		grace = DefaultRecoveryGrace
	}
	reconciler := NewReconciler(r.Client, r.Store)
	now := r.now()

	for _, job := range pending {
		report.Checked++
		if len(matchTransfers(job, transfers)) > 0 {
			if _, err := reconciler.applyTransfers(job, transfers); err != nil {
				return report, err
			}
			report.Active++
			continue
		}
		// slskd does not know this job. Either the bytes arrived before the
		// records expired, or the job never happened.
		if r.filesOnDisk(job) {
			if _, err := r.Store.Update(job.ID, func(j *Job) error {
				j.Status = JobCompletedPendingImport
				j.FailureReason = "recovered from disk: slskd no longer tracks this transfer"
				return nil
			}); err != nil {
				return report, err
			}
			report.Recovered = append(report.Recovered, job.ID)
			continue
		}
		// CreatedAt, not UpdatedAt: every state fold bumps UpdatedAt, so a job
		// that slskd keeps reporting would never age out of the grace window.
		if now.Sub(job.CreatedAt) < grace {
			report.LeftAlone = append(report.LeftAlone, job.ID)
			continue
		}
		if _, err := r.Store.Update(job.ID, func(j *Job) error {
			j.Status = JobInterrupted
			j.FailureReason = "slskd no longer tracks this transfer and no downloaded bytes are on disk (volatile restart or retention); re-enqueue to retry"
			return nil
		}); err != nil {
			return report, err
		}
		report.Interrupted = append(report.Interrupted, job.ID)
	}
	return report, nil
}

// filesOnDisk reports whether the pinned destination holds at least one
// non-empty regular file.
func (r *Recovery) filesOnDisk(job Job) bool {
	if strings.TrimSpace(r.Mount) == "" || job.DestinationSubdir == "" {
		return false
	}
	dir, err := ensurePathIsInside(r.Mount, filepath.Join(r.Mount, filepath.FromSlash(job.DestinationSubdir)))
	if err != nil {
		return false
	}
	found := false
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Mode().IsRegular() && info.Size() > 0 {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// Summary renders the report for a terminal.
func (rep RecoveryReport) Summary() []string {
	lines := []string{fmt.Sprintf("recovery: %d pending job(s) checked, %d active", rep.Checked, rep.Active)}
	if len(rep.Recovered) > 0 {
		lines = append(lines, fmt.Sprintf("  recovered from disk: %s", strings.Join(rep.Recovered, ", ")))
	}
	if len(rep.Interrupted) > 0 {
		lines = append(lines, fmt.Sprintf("  interrupted (re-enqueue to retry): %s", strings.Join(rep.Interrupted, ", ")))
	}
	if len(rep.LeftAlone) > 0 {
		lines = append(lines, fmt.Sprintf("  too new to judge: %s", strings.Join(rep.LeftAlone, ", ")))
	}
	return lines
}
