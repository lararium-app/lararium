package surface

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/lararium-app/lararium/internal/fasti"
)

// JobSummary represents a projection of a job row for socket list streaming (spec D8).
type JobSummary struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Schedule fasti.Schedule `json:"schedule"`
	Enabled  bool           `json:"enabled"`
	// Rev-5 presence rule: next_due is absent when the row is not armed
	// (paused/stale/terminal), matching the arming law — the summary has
	// no separate `paused` field, so the arm predicate (Enabled &&
	// !Paused) governs both enabled and next_due here.
	NextDue string `json:"next_due,omitempty"`
}

// GoalSummary represents a projection of a goal row for socket list streaming (spec D8).
type GoalSummary struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	TurnCount  int    `json:"turn_count"`
	TurnBudget int    `json:"turn_budget"`
	// Rev-5 presence rule: next_due absent unless armed (goal arm law:
	// only status "active" arms; paused/stale/terminal do not).
	NextDue string `json:"next_due,omitempty"`
}

type fastiSocketReq struct {
	Verb         string               `json:"verb"`
	Entity       string               `json:"entity"`
	ID           string               `json:"id,omitempty"`
	Name         string               `json:"name,omitempty"`
	Schedule     *fasti.Schedule      `json:"schedule,omitempty"`
	Prompt       *string              `json:"prompt,omitempty"`
	Session      string               `json:"session,omitempty"`
	Announce     string               `json:"announce,omitempty"`
	Quiet        *fasti.QuietOverride `json:"quiet,omitempty"`
	Enabled      *bool                `json:"enabled,omitempty"`
	Title        string               `json:"title,omitempty"`
	Requirements []string             `json:"requirements,omitempty"`
	Objectives   []string             `json:"objectives,omitempty"`
	Turns        int                  `json:"turns,omitempty"`
	TurnBudget   int                  `json:"turn_budget,omitempty"`
	Budget       int                  `json:"budget,omitempty"`
	StepAfter    string               `json:"step_after,omitempty"`
}

func (s *Server) handleFastiSocket(conn net.Conn, cmd string) {
	if s.Fasti == nil {
		fmt.Fprint(conn, "ERR fasti_disabled\n")
		return
	}

	payloadStr := strings.TrimSpace(strings.TrimPrefix(cmd, "FASTI"))
	if payloadStr == "" {
		fmt.Fprint(conn, "ERR bad_payload\n")
		return
	}

	sum := sha256.Sum256([]byte(payloadStr))
	payloadHash := hex.EncodeToString(sum[:])

	var req fastiSocketReq
	if err := json.Unmarshal([]byte(payloadStr), &req); err != nil {
		fmt.Fprint(conn, "ERR bad_payload\n")
		return
	}

	entity := strings.TrimSpace(req.Entity)
	if entity != "job" && entity != "goal" {
		fmt.Fprintf(conn, "ERR unknown_entity %s\n", req.Entity)
		return
	}

	if entity == "job" {
		s.handleFastiJobSocket(conn, req, payloadHash)
		return
	}
	s.handleFastiGoalSocket(conn, req)
}

func (s *Server) handleFastiJobSocket(conn net.Conn, req fastiSocketReq, payloadHash string) {
	switch strings.TrimSpace(req.Verb) {
	case "add":
		s.handleFastiJobAdd(conn, req)
	case "list":
		s.handleFastiJobList(conn)
	case "pause":
		s.handleFastiJobLifecycle(conn, req.ID, "pause")
	case "resume":
		s.handleFastiJobLifecycle(conn, req.ID, "resume")
	case "remove":
		s.handleFastiJobLifecycle(conn, req.ID, "remove")
	case "run":
		s.handleFastiJobLifecycle(conn, req.ID, "run")
	case "edit":
		s.handleFastiJobEdit(conn, req, payloadHash)
	default:
		fmt.Fprintf(conn, "ERR unknown_verb %s\n", req.Verb)
	}
}

func (s *Server) handleFastiJobAdd(conn net.Conn, req fastiSocketReq) {
	if req.Prompt == nil || strings.TrimSpace(*req.Prompt) == "" {
		fmt.Fprint(conn, "ERR bad_payload\n")
		return
	}
	if err := validatePromptRunes(*req.Prompt); err != nil {
		fmt.Fprintf(conn, "ERR %s\n", err)
		return
	}
	if req.Schedule == nil {
		fmt.Fprint(conn, "ERR bad_payload\n")
		return
	}
	if err := validateScheduleShape(*req.Schedule); err != nil {
		fmt.Fprintf(conn, "ERR %s\n", err)
		return
	}

	job := fasti.Job{
		Name:      req.Name,
		Schedule:  *req.Schedule,
		Prompt:    *req.Prompt,
		Session:   req.Session,
		Announce:  req.Announce,
		Quiet:     req.Quiet,
		CreatedBy: "user",
	}

	created, err := s.Fasti.AddJob(context.Background(), job)
	if err != nil {
		fmt.Fprintf(conn, "ERR %s\n", err)
		return
	}
	s.Fasti.Publish(fasti.Event{Kind: "job", Op: "upsert", Row: created})
	fmt.Fprintf(conn, "OK {\"id\":%q}\n", created.ID)
}

func (s *Server) handleFastiJobList(conn net.Conn) {
	jobs, err := s.Fasti.ListJobs(context.Background())
	if err != nil {
		fmt.Fprintf(conn, "ERR %s\n", err)
		return
	}
	for _, j := range jobs {
		row := JobSummary{
			ID:       j.ID,
			Name:     j.Name,
			Schedule: j.Schedule,
			Enabled:  j.Enabled && !j.Paused,
		}
		if row.Enabled {
			row.NextDue = j.NextDue
		}
		data, err := json.Marshal(row)
		if err == nil {
			fmt.Fprintf(conn, "%s\n", data)
		}
	}
}

func (s *Server) handleFastiJobLifecycle(conn net.Conn, id, op string) {
	if id == "" {
		fmt.Fprint(conn, "ERR bad_payload\n")
		return
	}
	ctx := context.Background()
	var err error
	switch op {
	case "pause":
		err = s.Fasti.PauseJob(ctx, id)
	case "resume":
		err = s.Fasti.ResumeJob(ctx, id)
	case "remove":
		err = s.Fasti.RemoveJob(ctx, id)
	case "run":
		err = s.Fasti.RunJob(ctx, id)
	}

	if errors.Is(err, fasti.ErrJobNotFound) {
		// D8 retry-idempotence law: lifecycle verbs are idempotent with
		// respect to the current state, never presence keys. For remove
		// the target state IS absence, so a retried remove — the exact
		// network-retry case the law names — acks as a no-op success.
		if op == "remove" {
			fmt.Fprintf(conn, "OK {\"id\":%q}\n", id)
			return
		}
		fmt.Fprintf(conn, "ERR not_found %s\n", id)
		return
	}
	if err != nil {
		fmt.Fprintf(conn, "ERR %s\n", err)
		return
	}

	if op == "remove" {
		s.Fasti.Publish(fasti.Event{Kind: "job", Op: "delete", Row: map[string]string{"id": id}})
	} else if j, gErr := s.Fasti.GetJob(ctx, id); gErr == nil {
		s.Fasti.Publish(fasti.Event{Kind: "job", Op: "upsert", Row: j})
	}
	fmt.Fprintf(conn, "OK {\"id\":%q}\n", id)
}

func (s *Server) handleFastiJobEdit(conn net.Conn, req fastiSocketReq, payloadHash string) {
	if req.ID == "" {
		fmt.Fprint(conn, "ERR bad_payload\n")
		return
	}
	ctx := context.Background()

	// D8 edit covers prompt/schedule/enabled only. Silently ignoring other
	// fields would ack a change that never lands; name and announce are set
	// at add time and are immutable on the socket edit path.
	if req.Name != "" || req.Announce != "" {
		fmt.Fprint(conn, "ERR edit supports prompt, schedule, and enabled only (name/announce are set at add)\n")
		return
	}

	replayed, err := s.Fasti.CheckEditDedup(ctx, req.ID, payloadHash)
	if err != nil {
		fmt.Fprintf(conn, "ERR %s\n", err)
		return
	}
	if replayed {
		fmt.Fprintf(conn, "OK {\"id\":%q,\"replayed\":true}\n", req.ID)
		return
	}

	job, err := s.Fasti.GetJob(ctx, req.ID)
	if errors.Is(err, fasti.ErrJobNotFound) {
		fmt.Fprintf(conn, "ERR not_found %s\n", req.ID)
		return
	}
	if err != nil {
		fmt.Fprintf(conn, "ERR %s\n", err)
		return
	}

	if req.Prompt != nil {
		if job.Enabled && !job.Paused {
			fmt.Fprint(conn, "ERR cannot update prompt while job is enabled\n")
			return
		}
		if err := validatePromptRunes(*req.Prompt); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err)
			return
		}
	}
	if req.Schedule != nil {
		if err := validateScheduleShape(*req.Schedule); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err)
			return
		}
	}

	if req.Prompt != nil || req.Schedule != nil {
		updated, err := s.Fasti.EditJob(ctx, req.ID, req.Prompt, req.Schedule)
		if err != nil {
			if errors.Is(err, fasti.ErrJobNotFound) {
				fmt.Fprintf(conn, "ERR not_found %s\n", req.ID)
				return
			}
			fmt.Fprintf(conn, "ERR %s\n", err)
			return
		}
		job = updated
	}

	if req.Enabled != nil {
		if *req.Enabled {
			err = s.Fasti.ResumeJob(ctx, req.ID)
		} else {
			err = s.Fasti.PauseJob(ctx, req.ID)
		}
		if errors.Is(err, fasti.ErrJobNotFound) {
			fmt.Fprintf(conn, "ERR not_found %s\n", req.ID)
			return
		}
		if err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err)
			return
		}
		if latest, gErr := s.Fasti.GetJob(ctx, req.ID); gErr == nil {
			job = latest
		}
	}

	_ = s.Fasti.RecordEditDedup(ctx, req.ID, payloadHash)
	s.Fasti.Publish(fasti.Event{Kind: "job", Op: "upsert", Row: job})
	fmt.Fprintf(conn, "OK {\"id\":%q}\n", req.ID)
}

func (s *Server) handleFastiGoalSocket(conn net.Conn, req fastiSocketReq) {
	switch strings.TrimSpace(req.Verb) {
	case "new", "add":
		s.handleFastiGoalNew(conn, req)
	case "list":
		s.handleFastiGoalList(conn)
	case "pause":
		s.handleFastiGoalLifecycle(conn, req.ID, "pause")
	case "resume":
		s.handleFastiGoalLifecycle(conn, req.ID, "resume")
	case "close":
		s.handleFastiGoalLifecycle(conn, req.ID, "close")
	case "cancel":
		s.handleFastiGoalLifecycle(conn, req.ID, "cancel")
	default:
		fmt.Fprintf(conn, "ERR unknown_verb %s\n", req.Verb)
	}
}

func (s *Server) handleFastiGoalNew(conn net.Conn, req fastiSocketReq) {
	reqs := req.Requirements
	if len(reqs) == 0 && len(req.Objectives) > 0 {
		reqs = req.Objectives
	}
	if len(reqs) < 1 {
		fmt.Fprint(conn, "ERR requirements must contain at least 1 item\n")
		return
	}
	title := req.Title
	if title == "" {
		title = reqs[0]
	}
	turns := req.TurnBudget
	if turns == 0 {
		turns = req.Turns
	}
	if turns == 0 {
		turns = req.Budget
	}
	// D10: turn_budget defaults to 25 and has no cap. The engine owns that
	// default (non-positive turns fall through to it); the surface must not
	// invent its own default or clamp, or the spec'd default never applies.
	if turns < 0 {
		fmt.Fprint(conn, "ERR turn_budget must be positive\n")
		return
	}
	stepAfter := 15 * time.Minute
	if strings.TrimSpace(req.StepAfter) != "" {
		d, err := time.ParseDuration(req.StepAfter)
		if err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err)
			return
		}
		stepAfter = d
	}
	spec := fasti.GoalSpec{
		Title:        title,
		SessionID:    req.Session,
		Requirements: reqs,
		TurnBudget:   turns,
		StepAfter:    stepAfter,
	}
	created, err := s.Fasti.AddGoal(context.Background(), spec)
	if err != nil {
		fmt.Fprintf(conn, "ERR %s\n", err)
		return
	}
	s.Fasti.Publish(fasti.Event{Kind: "goal", Op: "upsert", Row: created})
	fmt.Fprintf(conn, "OK {\"id\":%q}\n", created.ID)
}

func (s *Server) handleFastiGoalList(conn net.Conn) {
	goals, err := s.Fasti.ListGoals(context.Background())
	if err != nil {
		fmt.Fprintf(conn, "ERR %s\n", err)
		return
	}
	for _, g := range goals {
		row := GoalSummary{
			ID:         g.ID,
			Status:     g.Status,
			TurnCount:  g.TurnCount,
			TurnBudget: g.TurnBudget,
		}
		if g.Status == "active" {
			row.NextDue = g.NextDue
		}
		data, err := json.Marshal(row)
		if err == nil {
			fmt.Fprintf(conn, "%s\n", data)
		}
	}
}

func (s *Server) handleFastiGoalLifecycle(conn net.Conn, id, op string) {
	if id == "" {
		fmt.Fprint(conn, "ERR bad_payload\n")
		return
	}
	ctx := context.Background()
	var err error
	switch op {
	case "pause":
		err = s.Fasti.PauseGoal(ctx, id)
	case "resume":
		err = s.Fasti.ResumeGoal(ctx, id)
	case "close":
		err = s.Fasti.ForceCloseGoal(ctx, id)
	case "cancel":
		err = s.Fasti.CancelGoal(ctx, id)
	}

	if errors.Is(err, fasti.ErrGoalNotFound) {
		fmt.Fprintf(conn, "ERR not_found %s\n", id)
		return
	}
	if err != nil {
		fmt.Fprintf(conn, "ERR %s\n", err)
		return
	}
	if g, gErr := s.Fasti.GetGoal(ctx, id); gErr == nil {
		s.Fasti.Publish(fasti.Event{Kind: "goal", Op: "upsert", Row: g})
	}
	fmt.Fprintf(conn, "OK {\"id\":%q}\n", id)
}

// FastiMutationViaSocket sends a single FASTI mutation line and reads the single OK/ERR ack (spec D8).
func FastiMutationViaSocket(path string, reqJSON []byte) (string, error) {
	d := net.Dialer{Timeout: backupAckTimeout}
	conn, err := d.DialContext(context.Background(), "unix", path)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNotListening, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(backupAckTimeout))

	if _, err := fmt.Fprintf(conn, "FASTI %s\n", string(reqJSON)); err != nil {
		return "", err
	}

	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read fasti ack: %w", err)
	}
	line = trimCR(line)
	if strings.HasPrefix(line, "ERR ") {
		return "", errors.New(strings.TrimPrefix(line, "ERR "))
	}
	if line == "ERR" {
		return "", errors.New("unknown error")
	}
	if !strings.HasPrefix(line, "OK") {
		return "", fmt.Errorf("unexpected fasti response: %s", line)
	}
	return line, nil
}

// FastiStreamViaSocket sends a FASTI streaming command (such as list) and reads lines until EOF (spec D8).
func FastiStreamViaSocket(path string, reqJSON []byte) ([]string, error) {
	d := net.Dialer{Timeout: backupAckTimeout}
	conn, err := d.DialContext(context.Background(), "unix", path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotListening, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(backupAckTimeout))

	if _, err := fmt.Fprintf(conn, "FASTI %s\n", string(reqJSON)); err != nil {
		return nil, err
	}

	r := bufio.NewReader(conn)
	var lines []string
	first := true
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				if line != "" {
					lines = append(lines, trimCR(line))
				}
				break
			}
			return nil, fmt.Errorf("read fasti stream: %w", err)
		}
		line = trimCR(line)
		if first && strings.HasPrefix(line, "ERR ") {
			return nil, errors.New(strings.TrimPrefix(line, "ERR "))
		}
		first = false
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}
