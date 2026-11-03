package surface

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lararium-app/lararium/internal/fasti"
)

// dispatchFasti routes all /v1/fasti endpoints (spec D8).
func (s *Server) dispatchFasti(w http.ResponseWriter, r *http.Request) {
	if s.Fasti == nil {
		s.error(w, "fasti_disabled", http.StatusServiceUnavailable)
		return
	}

	p := r.URL.Path
	switch {
	case p == "/v1/fasti/state":
		if r.Method != http.MethodGet {
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.fastiStateGetHandler(w, r)
	case p == "/v1/fasti/events":
		if r.Method != http.MethodGet {
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.fastiEventsHandler(w, r)
	case p == "/v1/fasti/jobs":
		if r.Method != http.MethodPost {
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.fastiJobCreateHandler(w, r)
	case strings.HasPrefix(p, "/v1/fasti/jobs/"):
		s.dispatchFastiJob(w, r)
	case p == "/v1/fasti/goals":
		if r.Method != http.MethodPost {
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.fastiGoalCreateHandler(w, r)
	case strings.HasPrefix(p, "/v1/fasti/goals/"):
		s.dispatchFastiGoal(w, r)
	default:
		s.error(w, "not found", http.StatusNotFound)
	}
}

// dispatchFastiJob routes /v1/fasti/jobs/{id}/... endpoints.
func (s *Server) dispatchFastiJob(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/fasti/jobs/")
	if rest == "" {
		s.error(w, "not found", http.StatusNotFound)
		return
	}

	if strings.HasSuffix(rest, "/run") {
		id := strings.TrimSuffix(rest, "/run")
		if id == "" || strings.Contains(id, "/") {
			s.error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.fastiJobRunHandler(w, r, id)
		return
	}

	if strings.Contains(rest, "/") {
		s.error(w, "not found", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodPatch:
		s.fastiJobPatchHandler(w, r, rest)
	case http.MethodDelete:
		s.fastiJobDeleteHandler(w, r, rest)
	default:
		s.error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// dispatchFastiGoal routes /v1/fasti/goals/{id}/... endpoints.
func (s *Server) dispatchFastiGoal(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/fasti/goals/")
	if rest == "" {
		s.error(w, "not found", http.StatusNotFound)
		return
	}

	if strings.HasSuffix(rest, "/close") {
		id := strings.TrimSuffix(rest, "/close")
		if id == "" || strings.Contains(id, "/") {
			s.error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.fastiGoalCloseHandler(w, r, id)
		return
	}

	if strings.Contains(rest, "/") {
		s.error(w, "not found", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodPatch:
		s.fastiGoalPatchHandler(w, r, rest)
	default:
		s.error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// FastiHeartbeatState holds the serializable heartbeat frontmatter state (spec D6/D8).
type FastiHeartbeatState struct {
	Enabled bool   `json:"enabled"`
	Cadence string `json:"cadence"`
	Quiet   string `json:"quiet"`
}

// FastiStateResponse represents the full bootstrap snapshot (spec D8).
type FastiStateResponse struct {
	Jobs      []fasti.Job         `json:"jobs"`
	Goals     []fasti.Goal        `json:"goals"`
	Heartbeat FastiHeartbeatState `json:"heartbeat"`
	Noise     fasti.NoiseSnapshot `json:"noise"`
}

func (s *Server) fastiStateGetHandler(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.Fasti.ListJobs(r.Context())
	if err != nil {
		s.error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if jobs == nil {
		jobs = []fasti.Job{}
	}

	goals, err := s.Fasti.ListGoals(r.Context())
	if err != nil {
		s.error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if goals == nil {
		goals = []fasti.Goal{}
	}

	hb := s.Fasti.Heartbeat()
	hbState := FastiHeartbeatState{
		Enabled: hb.Enabled,
		Cadence: hb.Cadence,
		Quiet:   hb.Quiet,
	}

	noise := s.Fasti.NoiseSnapshot()

	writeJSONAny(w, http.StatusOK, FastiStateResponse{
		Jobs:      jobs,
		Goals:     goals,
		Heartbeat: hbState,
		Noise:     noise,
	})
}

type createJobRequest struct {
	Name     string               `json:"name"`
	Schedule fasti.Schedule       `json:"schedule"`
	Prompt   string               `json:"prompt"`
	Session  string               `json:"session,omitempty"`
	Announce string               `json:"announce,omitempty"`
	Quiet    *fasti.QuietOverride `json:"quiet,omitempty"`
}

func validateScheduleShape(sched fasti.Schedule) error {
	kind := strings.ToLower(strings.TrimSpace(sched.Kind))
	if kind != "cron" && kind != "interval" {
		return errors.New("schedule kind must be cron or interval")
	}
	if kind == "cron" && strings.TrimSpace(sched.Expr) == "" {
		return errors.New("cron schedule requires expr")
	}
	if kind == "interval" && strings.TrimSpace(sched.Every) == "" {
		return errors.New("interval schedule requires every")
	}
	return nil
}

func validatePromptRunes(prompt string) error {
	if utf8.RuneCountInString(prompt) > 1000 {
		return errors.New("prompt exceeds 1000 runes")
	}
	return fasti.ValidatePrompt(prompt)
}

func (s *Server) fastiJobCreateHandler(w http.ResponseWriter, r *http.Request) {
	var req createJobRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := validatePromptRunes(req.Prompt); err != nil {
		s.error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := validateScheduleShape(req.Schedule); err != nil {
		s.error(w, err.Error(), http.StatusBadRequest)
		return
	}

	job := fasti.Job{
		Name:      req.Name,
		Schedule:  req.Schedule,
		Prompt:    req.Prompt,
		Session:   req.Session,
		Announce:  req.Announce,
		Quiet:     req.Quiet,
		CreatedBy: "user",
	}

	created, err := s.Fasti.AddJob(r.Context(), job)
	if err != nil {
		s.error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.Fasti.Publish(fasti.Event{Kind: "job", Op: "upsert", Row: created})
	writeJSONAny(w, http.StatusCreated, created)
}

type patchJobRequest struct {
	Enabled  *bool           `json:"enabled,omitempty"`
	Schedule *fasti.Schedule `json:"schedule,omitempty"`
	Prompt   *string         `json:"prompt,omitempty"`
}

func (s *Server) fastiJobPatchHandler(w http.ResponseWriter, r *http.Request, id string) {
	job, err := s.Fasti.GetJob(r.Context(), id)
	if err != nil {
		if errors.Is(err, fasti.ErrJobNotFound) {
			s.error(w, "job not found", http.StatusNotFound)
			return
		}
		s.error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var req patchJobRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.Prompt != nil {
		if job.Enabled && !job.Paused {
			s.error(w, "cannot update prompt while job is enabled", http.StatusBadRequest)
			return
		}
		if err := validatePromptRunes(*req.Prompt); err != nil {
			s.error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	if req.Schedule != nil {
		if err := validateScheduleShape(*req.Schedule); err != nil {
			s.error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	updated, err := s.applyJobPatch(r.Context(), job, req)
	if err != nil {
		if errors.Is(err, fasti.ErrJobNotFound) {
			s.error(w, "job not found", http.StatusNotFound)
			return
		}
		s.error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.Fasti.Publish(fasti.Event{Kind: "job", Op: "upsert", Row: updated})
	writeJSONAny(w, http.StatusOK, updated)
}

func (s *Server) applyJobPatch(ctx context.Context, job fasti.Job, req patchJobRequest) (fasti.Job, error) {
	id := job.ID
	if req.Prompt != nil || req.Schedule != nil {
		// One atomic engine edit (single-writer law): identity, history and
		// pause state survive; the engine applies the D9 adoption law
		// (created_by → user:<surface> + fasti_job_adopted audit) itself.
		updated, err := s.Fasti.EditJob(ctx, id, req.Prompt, req.Schedule)
		if err != nil {
			return fasti.Job{}, err
		}
		job = updated
	}

	if req.Enabled != nil {
		if *req.Enabled {
			if err := s.Fasti.ResumeJob(ctx, id); err != nil {
				return fasti.Job{}, err
			}
		} else {
			if err := s.Fasti.PauseJob(ctx, id); err != nil {
				return fasti.Job{}, err
			}
		}
		latest, err := s.Fasti.GetJob(ctx, id)
		if err == nil {
			job = latest
		}
	}

	return job, nil
}

func (s *Server) fastiJobDeleteHandler(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.Fasti.RemoveJob(r.Context(), id); err != nil {
		if errors.Is(err, fasti.ErrJobNotFound) {
			s.error(w, "job not found", http.StatusNotFound)
			return
		}
		s.error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.Fasti.Publish(fasti.Event{Kind: "job", Op: "delete", Row: map[string]string{"id": id}})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) fastiJobRunHandler(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.Fasti.RunJob(r.Context(), id); err != nil {
		if errors.Is(err, fasti.ErrJobNotFound) {
			s.error(w, "job not found", http.StatusNotFound)
			return
		}
		s.error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if job, err := s.Fasti.GetJob(r.Context(), id); err == nil {
		s.Fasti.Publish(fasti.Event{Kind: "job", Op: "upsert", Row: job})
	}

	writeJSONAny(w, http.StatusAccepted, map[string]string{"id": id, "status": "accepted"})
}

type createGoalRequest struct {
	Title        string   `json:"title"`
	Session      string   `json:"session,omitempty"`
	Requirements []string `json:"requirements"`
	TurnBudget   *int     `json:"turn_budget,omitempty"`
	StepAfter    string   `json:"step_after,omitempty"`
}

func (s *Server) fastiGoalCreateHandler(w http.ResponseWriter, r *http.Request) {
	var req createGoalRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if len(req.Requirements) < 1 {
		s.error(w, "requirements must contain at least 1 item", http.StatusBadRequest)
		return
	}

	spec := fasti.GoalSpec{
		Title:        req.Title,
		SessionID:    req.Session,
		Requirements: req.Requirements,
	}

	if req.TurnBudget != nil {
		spec.TurnBudget = *req.TurnBudget
	} else {
		spec.TurnBudget = 25
	}

	if strings.TrimSpace(req.StepAfter) != "" {
		d, err := time.ParseDuration(req.StepAfter)
		if err != nil {
			s.error(w, "invalid step_after duration: "+err.Error(), http.StatusBadRequest)
			return
		}
		spec.StepAfter = d
	} else {
		spec.StepAfter = 15 * time.Minute
	}

	goal, err := s.Fasti.AddGoal(r.Context(), spec)
	if err != nil {
		s.error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.Fasti.Publish(fasti.Event{Kind: "goal", Op: "upsert", Row: goal})
	writeJSONAny(w, http.StatusCreated, goal)
}

type patchGoalRequest struct {
	Status     *string `json:"status,omitempty"`
	Budget     *int    `json:"budget,omitempty"`
	TurnBudget *int    `json:"turn_budget,omitempty"`
}

func (s *Server) fastiGoalPatchHandler(w http.ResponseWriter, r *http.Request, id string) {
	var req patchGoalRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.Status != nil {
		switch *req.Status {
		case "paused":
			if err := s.Fasti.PauseGoal(r.Context(), id); err != nil {
				s.handleGoalError(w, err)
				return
			}
		case "active":
			if err := s.Fasti.ResumeGoal(r.Context(), id); err != nil {
				s.handleGoalError(w, err)
				return
			}
		case "failed":
			if err := s.Fasti.CancelGoal(r.Context(), id); err != nil {
				s.handleGoalError(w, err)
				return
			}
		default:
			s.error(w, "status must be paused, active, or failed", http.StatusBadRequest)
			return
		}
	}

	var b *int
	if req.Budget != nil {
		b = req.Budget
	} else if req.TurnBudget != nil {
		b = req.TurnBudget
	}

	if b != nil {
		if *b <= 0 {
			s.error(w, "budget must be positive", http.StatusBadRequest)
			return
		}
		if err := s.Fasti.SetGoalBudget(r.Context(), id, *b); err != nil {
			s.handleGoalError(w, err)
			return
		}
	}

	goal, err := s.Fasti.GetGoal(r.Context(), id)
	if err != nil {
		s.handleGoalError(w, err)
		return
	}

	s.Fasti.Publish(fasti.Event{Kind: "goal", Op: "upsert", Row: goal})
	writeJSONAny(w, http.StatusOK, goal)
}

type closeGoalRequest struct {
	Mode string `json:"mode"`
}

func (s *Server) fastiGoalCloseHandler(w http.ResponseWriter, r *http.Request, id string) {
	var req closeGoalRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.Mode != "user_force" {
		s.error(w, "only mode user_force is permitted", http.StatusBadRequest)
		return
	}

	if err := s.Fasti.ForceCloseGoal(r.Context(), id); err != nil {
		s.handleGoalError(w, err)
		return
	}

	goal, err := s.Fasti.GetGoal(r.Context(), id)
	if err == nil {
		s.Fasti.Publish(fasti.Event{Kind: "goal", Op: "upsert", Row: goal})
	}
	writeJSONAny(w, http.StatusOK, goal)
}

func (s *Server) handleGoalError(w http.ResponseWriter, err error) {
	if errors.Is(err, fasti.ErrGoalNotFound) {
		s.error(w, "goal not found", http.StatusNotFound)
		return
	}
	s.error(w, err.Error(), http.StatusBadRequest)
}
