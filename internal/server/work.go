package server

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Personed/Persona/internal/agent"
	"github.com/Personed/Persona/internal/mcp"
	"github.com/Personed/Persona/internal/memory"
	"github.com/Personed/Persona/internal/routines"
	"github.com/Personed/Persona/internal/rpc"
	"github.com/Personed/Persona/internal/skills"
)

// Work holds the services behind the work panel: Bot memory, routines and skills.
type Work struct {
	Memory   *memory.Service
	Routines *routines.Service
	Skills   *skills.Service
}

// RegisterWork registers the work procedures, as workRouter in
// apps/api/src/work.ts. work.prompt reads the prompt a native task was
// started with, so it needs a native runtime; work.routines.test needs the
// routine runner (Routines.RunNow). work.skills.save, preview and restore
// stay unregistered until SKILL.md front matter is parsed with the YAML
// failsafe schema the TypeScript service uses.
func RegisterWork(s *rpc.Server, a *agent.Agent, rt *agent.Runtime, w Work) {
	r := workRoutes{agentRoutes: agentRoutes{a: a, rt: rt}, w: w}
	s.Handle("work.get", func(ctx context.Context, input json.RawMessage) (any, error) {
		id, err := r.bot(ctx, input)
		if err != nil {
			return nil, err
		}
		return r.read(ctx, id)
	})
	s.HandleStream("work.subscribe", r.subscribe)
	if rt != nil && rt.Native() {
		s.Handle("work.prompt", r.prompt)
	}
	s.Handle("work.memory.save", r.saveMemory)
	s.Handle("work.memory.delete", r.deleteMemory)
	s.Handle("work.memory.review.setEnabled", r.setReview)
	s.Handle("work.routines.save", r.saveRoutine)
	s.Handle("work.routines.enable", r.enableRoutine)
	s.Handle("work.routines.delete", r.deleteRoutine)
	if w.Routines.RunNow != nil {
		s.Handle("work.routines.test", r.testRoutine)
	}
	s.Handle("work.skills.enable", r.enableSkill)
	s.Handle("work.skills.delete", r.deleteSkill)
	s.Handle("work.skills.export", r.exportSkill)
}

type workRoutes struct {
	agentRoutes
	w Work
}

var okResult = map[string]bool{"ok": true}

// workError converts a work package Error into its oRPC error, as the
// TypeScript routers convert WorkError.
func workError(err error) error {
	var (
		m  *memory.Error
		ro *routines.Error
		sk *skills.Error
		mc *mcp.Error
	)
	switch {
	case errors.As(err, &m):
		return rpc.NewError(string(m.Code), m.Message)
	case errors.As(err, &ro):
		return rpc.NewError(string(ro.Code), ro.Message)
	case errors.As(err, &sk):
		return rpc.NewError(string(sk.Code), sk.Message)
	case errors.As(err, &mc):
		return rpc.NewError(string(mc.Code), mc.Message)
	}
	return rpcError(err)
}

// target decodes input into v after checking its Bot, as access in work.ts.
func (r workRoutes) target(ctx context.Context, input json.RawMessage, v any) (int64, error) {
	id, err := r.bot(ctx, input)
	if err != nil {
		return 0, err
	}
	return id, json.Unmarshal(input, v)
}

// read is botWork.read in packages/agent/src/work/index.ts.
func (r workRoutes) read(ctx context.Context, botID int64) (any, error) {
	items, err := r.w.Memory.List(ctx, botID)
	if err != nil {
		return nil, err
	}
	enabled, err := r.w.Memory.ReviewEnabled(ctx, botID)
	if err != nil {
		return nil, err
	}
	all, err := r.w.Skills.List(ctx)
	if err != nil {
		return nil, err
	}
	listing, err := r.w.Routines.List(ctx, botID)
	if err != nil {
		return nil, err
	}
	engine, _, err := agent.Snapshot(ctx, r.a.Harness, agent.EngineDoc, botID)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"memory":              items,
		"memoryUsage":         memory.Usage(items),
		"memoryReviewEnabled": enabled,
		"skills":              all,
		"routines":            map[string]any{"items": listing.Items, "runs": listing.Runs},
	}
	if native, ok := engine.Runtime["nativeSkills"]; ok {
		out["nativeSkills"] = native
	}
	return out, nil
}

func (r workRoutes) subscribe(ctx context.Context, input json.RawMessage, send func(any) error) (any, error) {
	id, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	commits, err := r.a.Harness.Watch(ctx)
	if err != nil {
		return nil, err
	}
	return rpc.WatchValues(ctx, commits, func(ctx context.Context) (any, error) { return r.read(ctx, id) }, send)
}

// prompt is Runtime.prompt in packages/agent/src/runtime: the prompt the
// Bot's latest native task started with.
func (r workRoutes) prompt(ctx context.Context, input json.RawMessage) (any, error) {
	id, err := r.bot(ctx, input)
	if err != nil {
		return nil, err
	}
	engine, _, err := agent.Snapshot(ctx, r.a.Harness, agent.EngineDoc, id)
	if err != nil {
		return nil, err
	}
	prompt := map[string]any{}
	if err := json.Unmarshal(engine.Prompt, &prompt); err != nil || prompt == nil {
		return map[string]any{"text": "", "sections": map[string]any{}, "tools": []any{}, "prepared": false}, nil
	}
	prompt["prepared"] = true
	return prompt, nil
}

func (r workRoutes) saveMemory(ctx context.Context, input json.RawMessage) (any, error) {
	var in struct {
		ID        string `json:"id"`
		Revision  int    `json:"revision"`
		Text      string `json:"text"`
		Category  string `json:"category"`
		Shared    bool   `json:"shared"`
		RequestID string `json:"requestId"`
	}
	id, err := r.target(ctx, input, &in)
	if err != nil {
		return nil, err
	}
	entry, err := r.w.Memory.Save(ctx, id, memory.Input{ID: in.ID, Text: in.Text, Category: in.Category,
		Shared: &in.Shared, Revision: in.Revision, RequestID: in.RequestID}, memory.UserSource)
	if err != nil {
		return nil, workError(err)
	}
	return entry, nil
}

type itemTarget struct {
	ID        string `json:"id"`
	Revision  int    `json:"revision"`
	Enabled   bool   `json:"enabled"`
	RequestID string `json:"requestId"`
}

func (r workRoutes) deleteMemory(ctx context.Context, input json.RawMessage) (any, error) {
	var in itemTarget
	id, err := r.target(ctx, input, &in)
	if err != nil {
		return nil, err
	}
	if err := r.w.Memory.Delete(ctx, id, in.ID, in.Revision, memory.UserSource); err != nil {
		return nil, workError(err)
	}
	return okResult, nil
}

// setReview turns background review on or off. The Go runtime runs no
// background review yet, so there is no running review to cancel.
func (r workRoutes) setReview(ctx context.Context, input json.RawMessage) (any, error) {
	var in itemTarget
	id, err := r.target(ctx, input, &in)
	if err != nil {
		return nil, err
	}
	if err := r.w.Memory.SetReviewEnabled(ctx, id, in.Enabled); err != nil {
		return nil, workError(err)
	}
	return map[string]bool{"enabled": in.Enabled}, nil
}

func (r workRoutes) saveRoutine(ctx context.Context, input json.RawMessage) (any, error) {
	var in routines.Input
	id, err := r.target(ctx, input, &in)
	if err != nil {
		return nil, err
	}
	item, err := r.w.Routines.Save(ctx, id, in)
	if err != nil {
		return nil, workError(err)
	}
	return item, nil
}

func (r workRoutes) enableRoutine(ctx context.Context, input json.RawMessage) (any, error) {
	var in itemTarget
	id, err := r.target(ctx, input, &in)
	if err != nil {
		return nil, err
	}
	if err := r.w.Routines.Enable(ctx, id, in.ID, in.Enabled, in.Revision, in.RequestID); err != nil {
		return nil, workError(err)
	}
	return okResult, nil
}

func (r workRoutes) deleteRoutine(ctx context.Context, input json.RawMessage) (any, error) {
	var in itemTarget
	id, err := r.target(ctx, input, &in)
	if err != nil {
		return nil, err
	}
	if err := r.w.Routines.Delete(ctx, id, in.ID, in.Revision); err != nil {
		return nil, workError(err)
	}
	return okResult, nil
}

func (r workRoutes) testRoutine(ctx context.Context, input json.RawMessage) (any, error) {
	var in itemTarget
	id, err := r.target(ctx, input, &in)
	if err != nil {
		return nil, err
	}
	task, err := r.w.Routines.Test(ctx, id, in.ID, in.RequestID)
	if err != nil {
		return nil, workError(err)
	}
	return map[string]any{"taskId": task}, nil
}

func (r workRoutes) enableSkill(ctx context.Context, input json.RawMessage) (any, error) {
	var in itemTarget
	id, err := r.target(ctx, input, &in)
	if err != nil {
		return nil, err
	}
	if err := r.w.Skills.Enable(ctx, id, in.ID, in.Enabled, in.Revision, in.RequestID); err != nil {
		return nil, workError(err)
	}
	return okResult, nil
}

func (r workRoutes) deleteSkill(ctx context.Context, input json.RawMessage) (any, error) {
	var in itemTarget
	if _, err := r.target(ctx, input, &in); err != nil {
		return nil, err
	}
	if err := r.w.Skills.Delete(ctx, in.ID, in.Revision); err != nil {
		return nil, workError(err)
	}
	return okResult, nil
}

func (r workRoutes) exportSkill(ctx context.Context, input json.RawMessage) (any, error) {
	var in struct {
		ID   string `json:"id"`
		Hash string `json:"hash"`
	}
	if _, err := r.target(ctx, input, &in); err != nil {
		return nil, err
	}
	export, err := r.w.Skills.Export(ctx, in.ID, in.Hash)
	if err != nil {
		return nil, workError(err)
	}
	return export, nil
}
