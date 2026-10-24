package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
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

// handleAction runs button clicks: pending confirmations and the stateless
// task:<verb>:<repo>:<id> buttons attached to alerts and task views.
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
	case "task":
		parts := strings.Split(rest, ":")
		if len(parts) != 3 {
			return "", nil, fmt.Errorf("Malformed task action")
		}
		verb, repo, id := parts[0], parts[1], parts[2]
		app := s.fleet.For(repo)
		if app == nil {
			return "", nil, fmt.Errorf("Repository %s is not served by this instance", repo)
		}
		switch verb {
		case "view":
			text, buttons, _, err := s.taskDetail(ctx, inv, app, []string{id})
			return text, buttons, err
		case "retry", "cancel":
			text, buttons, _, err := s.taskConfirm(ctx, inv, app, verb, []string{id})
			return text, buttons, err
		}
	}
	return "", nil, fmt.Errorf("Unknown action; try `help`")
}
