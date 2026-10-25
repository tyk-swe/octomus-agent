package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// confirm parks a mutating action behind a one-shot Confirm button. The
// parked action is bound to the operator who asked for it and expires
// quickly; it lives only in memory, so a restart simply means asking again.
func (s *Service) confirm(inv Invocation, prompt string, run func(context.Context) (string, error)) (string, []Button, string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, "", err
	}
	token := hex.EncodeToString(raw[:])
	s.mu.Lock()
	now := s.now()
	for k, v := range s.pending {
		if now.After(v.expires) {
			delete(s.pending, k)
		}
	}
	if len(s.pending) >= maxPending {
		s.mu.Unlock()
		return "", nil, "", fmt.Errorf("Too many actions are awaiting confirmation; confirm or cancel one first")
	}
	s.pending[token] = &pendingAction{userID: inv.UserID, expires: now.Add(pendingTTL), run: run}
	s.mu.Unlock()
	return prompt, []Button{
		{Label: "Confirm", Value: "confirm:" + token},
		{Label: "Cancel", Value: "cancel:" + token},
	}, "", nil
}

// takePending consumes a confirmation token only for the operator who
// created it; another operator's click leaves it in place.
func (s *Service) takePending(token, userID string) *pendingAction {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending[token]
	if p == nil {
		return nil
	}
	if s.now().After(p.expires) {
		delete(s.pending, token)
		return nil
	}
	if p.userID != userID {
		return nil
	}
	delete(s.pending, token)
	return p
}

// pendingOwnedByOther reports whether a live confirmation belongs to a
// different operator, without consuming it. Adapters whose component
// replies would replace a shared message use this to answer the other
// operator privately and leave the creator's buttons intact.
func (s *Service) pendingOwnedByOther(token, userID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending[token]
	if p == nil {
		return false
	}
	if s.now().After(p.expires) {
		delete(s.pending, token)
		return false
	}
	return p.userID != userID
}

// taskActionValue builds a task button's value. It deliberately omits
// the repository: a full OWNER/REPO plus a UUID can exceed Discord's
// 100-character custom_id limit, and the owning repository is resolved
// from the record itself (Fleet.ForRecord) when the button is clicked.
func taskActionValue(verb, id string) string {
	return "task:" + verb + ":" + id
}

// handleAction runs button clicks: pending confirmations, digest page
// navigation, and the stateless task:<verb>:<id> buttons attached to
// alerts and task views.
func (s *Service) handleAction(ctx context.Context, inv Invocation) (string, []Button, error) {
	kind, rest, _ := strings.Cut(inv.Action, ":")
	switch kind {
	case "confirm", "cancel":
		p := s.takePending(rest, inv.UserID)
		if p == nil {
			return "That confirmation has expired; run the command again.", nil, nil
		}
		if kind == "cancel" {
			return "Cancelled.", nil, nil
		}
		text, err := p.run(ctx)
		return text, nil, err
	case "digest":
		page, err := strconv.Atoi(rest)
		if err != nil {
			return "", nil, fmt.Errorf("Malformed digest action")
		}
		return s.digestPage(ctx, page)
	case "task":
		parts := strings.Split(rest, ":")
		var verb, repo, id string
		switch len(parts) {
		case 2: // task:<verb>:<id> — the owner resolves from the record
			verb, id = parts[0], parts[1]
		case 3: // legacy task:<verb>:<repo>:<id> on already-posted alerts
			verb, repo, id = parts[0], parts[1], parts[2]
		default:
			return "", nil, fmt.Errorf("Malformed task action")
		}
		app, err := s.fleet.ForRecord("task", id, repo)
		if err != nil {
			return "", nil, err
		}
		var text string
		var buttons []Button
		switch verb {
		case "view":
			text, buttons, _, err = s.taskDetail(ctx, inv, app, []string{id})
		case "retry", "cancel":
			text, buttons, _, err = s.taskConfirm(ctx, inv, app, verb, []string{id})
		default:
			return "", nil, fmt.Errorf("Unknown action; try `help`")
		}
		if err == nil {
			// Button flows update thread context exactly like the
			// equivalent text commands, so follow-ups keep addressing
			// the viewed task's repository.
			s.remember(inv, app, id)
		}
		return text, buttons, err
	}
	return "", nil, fmt.Errorf("Unknown action; try `help`")
}
