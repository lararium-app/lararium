package fasti

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ProposalOutcome enumerates the terminal settlements of a model-proposed job card (spec D5, V-Pf).
type ProposalOutcome int

const (
	// ProposalAllow flips the proposed job to enabled, arms next_due, publishes SSE upsert, and audits fasti_job_enabled.
	ProposalAllow ProposalOutcome = iota
	// ProposalDeny removes the proposed job row and audits fasti_job_removed.
	ProposalDeny
	// ProposalTimeout removes the proposed job row and audits fasti_job_timeout.
	ProposalTimeout
	// ProposalRollback removes the proposed job row when approval card registration fails and audits fasti_job_removed.
	ProposalRollback
)

type cmdProposeJob struct {
	job  Job
	resp chan addJobResp
}

type cmdSettleProposal struct {
	id      string
	outcome ProposalOutcome
	via     string
	resp    chan error
}

// SPEC-SILENCE: the spec requires boot sweep to remove model-proposed enabled:false
// jobs whose card is gone on daemon restart while leaving user-created disabled jobs
// and paused jobs untouched. Model proposals are created enabled:false, paused:false,
// next_due:"" with created_by:"model:<session>", and only receive a non-empty next_due
// when an allow verdict flips enabled:true. Matching !Enabled && !Paused &&
// NextDue == "" && strings.HasPrefix(CreatedBy, "model:") uniquely identifies
// unsettled model proposals without requiring schema migration.
func isUnsettledModelProposal(job Job) bool {
	return !job.Enabled && !job.Paused && job.NextDue == "" && strings.HasPrefix(job.CreatedBy, "model:")
}

// sweepUnsettledProposalsLocked removes any model-proposed job that never received
// a card verdict before daemon restart, emitting fasti_job_timeout per row (spec D5).
func (e *Engine) sweepUnsettledProposalsLocked() {
	kept := e.jobs[:0]
	for _, job := range e.jobs {
		if isUnsettledModelProposal(job) {
			sched := job.Schedule
			_, _ = e.journal.Append(JournalEvent{
				Type:      "fasti_job_timeout",
				ID:        job.ID,
				JobID:     job.ID,
				Name:      job.Name,
				JobName:   job.Name,
				Schedule:  &sched,
				CreatedBy: job.CreatedBy,
				SessionID: job.Session,
				Via:       "timeout",
			})
			continue
		}
		kept = append(kept, job)
	}
	e.jobs = kept
}

func (e *Engine) handleProposeJob(cmd cmdProposeJob) {
	job := cmd.job
	now := e.clock.Now().UTC()

	if job.ID == "" {
		job.ID = NewJobID(now)
	}
	if job.Session == "" || job.Session == "new" {
		job.Session = "cron-" + job.ID
	}
	if job.CreatedBy == "" || job.CreatedBy == "model" {
		job.CreatedBy = "model:main"
	}
	if !strings.HasPrefix(job.CreatedBy, "model:") {
		cmd.resp <- addJobResp{err: fmt.Errorf("propose job requires model:<session> created_by, got %q", job.CreatedBy)}
		return
	}

	// Spec D5: proposed row starts disabled with no armed due until approved.
	job.Enabled = false
	job.Paused = false
	job.NextDue = ""

	if err := job.Validate(); err != nil {
		cmd.resp <- addJobResp{err: err}
		return
	}
	if err := e.validatePromptSecrets(job.Prompt); err != nil {
		cmd.resp <- addJobResp{err: err}
		return
	}
	if _, err := e.calculateNextDue(&job, now); err != nil {
		cmd.resp <- addJobResp{err: fmt.Errorf("calculate initial next due: %w", err)}
		return
	}

	for _, existing := range e.jobs {
		if existing.ID == job.ID {
			cmd.resp <- addJobResp{err: fmt.Errorf("job id %q already exists", job.ID)}
			return
		}
	}

	e.jobs = append(e.jobs, job)
	if err := SaveJobs(e.jobsPath, e.jobs); err != nil {
		e.jobs = e.jobs[:len(e.jobs)-1]
		cmd.resp <- addJobResp{err: fmt.Errorf("save jobs: %w", err)}
		return
	}

	sched := job.Schedule
	_, _ = e.journal.Append(JournalEvent{
		Type:      "fasti_job_created",
		ID:        job.ID,
		JobID:     job.ID,
		Name:      job.Name,
		JobName:   job.Name,
		Schedule:  &sched,
		CreatedBy: job.CreatedBy,
		SessionID: job.Session,
	})

	cmd.resp <- addJobResp{job: job}
}

func (e *Engine) handleSettleProposal(cmd cmdSettleProposal) {
	idx := -1
	for i, j := range e.jobs {
		if j.ID == cmd.id {
			idx = i
			break
		}
	}
	if idx == -1 {
		cmd.resp <- ErrJobNotFound
		return
	}

	switch cmd.outcome {
	case ProposalAllow:
		job := &e.jobs[idx]
		now := e.clock.Now().UTC()
		res, err := e.calculateNextDue(job, now)
		if err != nil {
			cmd.resp <- fmt.Errorf("calculate initial next due: %w", err)
			return
		}
		job.Enabled = true
		job.Paused = false
		job.NextDue = res.Due.Format(time.RFC3339)

		e.armTimerLocked(job, res.Due, res.IsDstGap, res.SkippedDue)
		e.saveAndPublishJobLocked(job)

		via := cmd.via
		if via == "" {
			via = "allow"
		}
		sched := job.Schedule
		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_job_enabled",
			ID:        job.ID,
			JobID:     job.ID,
			Name:      job.Name,
			JobName:   job.Name,
			Schedule:  &sched,
			CreatedBy: job.CreatedBy,
			SessionID: job.Session,
			DueAt:     job.NextDue,
			Via:       via,
		})
		cmd.resp <- nil

	case ProposalDeny, ProposalRollback:
		removed := e.removeJobAtLocked(idx)
		if err := SaveJobs(e.jobsPath, e.jobs); err != nil {
			cmd.resp <- fmt.Errorf("save jobs: %w", err)
			return
		}
		// SPEC-SILENCE: D5 mentions settling via for card outcomes while V-Pf
		// specifies fasti_job_removed{via:"deny"} when a proposal card is denied;
		// we stamp via="deny" on card denial and via="rollback" on card-reg failure.
		via := "deny"
		if cmd.outcome == ProposalRollback {
			via = "rollback"
		}
		sched := removed.Schedule
		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_job_removed",
			ID:        removed.ID,
			JobID:     removed.ID,
			Name:      removed.Name,
			JobName:   removed.Name,
			Schedule:  &sched,
			CreatedBy: removed.CreatedBy,
			SessionID: removed.Session,
			Via:       via,
		})
		cmd.resp <- nil

	case ProposalTimeout:
		removed := e.removeJobAtLocked(idx)
		if err := SaveJobs(e.jobsPath, e.jobs); err != nil {
			cmd.resp <- fmt.Errorf("save jobs: %w", err)
			return
		}
		sched := removed.Schedule
		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_job_timeout",
			ID:        removed.ID,
			JobID:     removed.ID,
			Name:      removed.Name,
			JobName:   removed.Name,
			Schedule:  &sched,
			CreatedBy: removed.CreatedBy,
			SessionID: removed.Session,
			Via:       "timeout",
		})
		cmd.resp <- nil

	default:
		cmd.resp <- fmt.Errorf("unknown proposal outcome %d", cmd.outcome)
	}
}

func (e *Engine) removeJobAtLocked(idx int) Job {
	removed := e.jobs[idx]
	if t, ok := e.activeTimers[removed.ID]; ok && t != nil {
		t.Stop()
		delete(e.activeTimers, removed.ID)
	}
	if t, ok := e.retryTimers[removed.ID]; ok && t != nil {
		t.Stop()
		delete(e.retryTimers, removed.ID)
	}
	e.jobs = append(e.jobs[:idx], e.jobs[idx+1:]...)
	return removed
}

// ProposeJob persists a disabled model-authored job proposal and emits fasti_job_created (spec D5).
func (e *Engine) ProposeJob(ctx context.Context, job Job) (Job, error) {
	respCh := make(chan addJobResp, 1)
	select {
	case e.cmdCh <- cmdProposeJob{job: job, resp: respCh}:
	case <-ctx.Done():
		return Job{}, ctx.Err()
	case <-e.stopCh:
		return Job{}, errors.New("engine stopped")
	}

	select {
	case res := <-respCh:
		return res.job, res.err
	case <-ctx.Done():
		return Job{}, ctx.Err()
	case <-e.stopCh:
		return Job{}, errors.New("engine stopped")
	}
}

// SettleProposal applies the verdict (allow, deny, timeout, or rollback) for a proposed job (spec D5, V-Pf).
func (e *Engine) SettleProposal(ctx context.Context, id string, outcome ProposalOutcome, via string) error {
	respCh := make(chan error, 1)
	select {
	case e.cmdCh <- cmdSettleProposal{id: id, outcome: outcome, via: via, resp: respCh}:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}

	select {
	case err := <-respCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}
}
