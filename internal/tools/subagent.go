package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/matjam/faultline/internal/llm"
	"github.com/matjam/faultline/internal/subagent"
)

// subagentToolDefs returns the tool definitions for subagent
// delegation. The returned slice depends on the Executor's Mode:
//
//   - ModePrimary: subagent_run, subagent_spawn, subagent_status,
//     subagent_cancel — but only when the Manager is wired and at
//     least one profile is available.
//   - ModeSubagent: subagent_report — but only when a report sink
//     is wired (i.e. the spawnFn that constructed this Executor
//     supplied one).
//
// Returning an empty slice (rather than not advertising) keeps the
// caller's append loop in ToolDefs symmetric with the skill_* path.
func (te *Executor) subagentToolDefs() []llm.Tool {
	switch te.mode {
	case ModePrimary:
		if te.subagentMgr == nil {
			return nil
		}
		profiles := te.subagentMgr.Profiles()
		if len(profiles) == 0 {
			return nil
		}
		return te.primarySubagentTools(profiles)
	case ModeSubagent:
		if te.subagentReportFn == nil {
			return nil
		}
		return []llm.Tool{te.subagentReportToolDef()}
	default:
		return nil
	}
}

// primarySubagentTools builds the five primary-side tool defs. The
// "profile" parameter on subagent_run and subagent_spawn is constrained
// to a JSON-schema enum of currently-configured profile names, so the
// model can't hallucinate one.
func (te *Executor) primarySubagentTools(profiles []subagent.Profile) []llm.Tool {
	names := make([]string, len(profiles))
	for i, p := range profiles {
		names[i] = p.Name
	}
	sort.Strings(names)

	profileEnum := make([]interface{}, len(names))
	for i, n := range names {
		profileEnum[i] = n
	}

	// Render the profile catalog as a description hint so the model
	// knows when to pick which profile. Format: "  - <name>: <purpose>"
	var catalog strings.Builder
	catalog.WriteString("\n\nAvailable profiles:")
	for _, p := range profiles {
		purpose := strings.TrimSpace(p.Purpose)
		if purpose == "" {
			purpose = "(no purpose configured)"
		}
		fmt.Fprintf(&catalog, "\n  - %s: %s", p.Name, purpose)
	}

	runDesc := "Delegate work to a subagent and BLOCK until it returns its report. Use this when you need the subagent's answer to continue. The subagent runs a fresh agent loop with the chosen profile's LLM endpoint and has the same tools you do (minus sleep, update_*, and nested subagent_*); it cannot see your conversation log, so put EVERYTHING it needs to know in `prompt`. The return value is the subagent's free-form report (whatever it produced via subagent_report)." + catalog.String()

	spawnDesc := "Delegate work to a subagent CONCURRENTLY. Returns immediately with a work_id; the subagent runs in the background and its report arrives in your context the same way operator messages do, prefixed with [Subagent report — work_id=...]. Use this when you can keep working while the subagent thinks. Same context-isolation rules as subagent_run: the subagent cannot see your conversation log, so put everything it needs in `prompt`." + catalog.String()

	return []llm.Tool{
		{
			Type: llm.ToolTypeFunction,
			Function: &llm.FunctionDef{
				Name:        "subagent_run",
				Description: runDesc,
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"profile": map[string]interface{}{
							"type":        "string",
							"description": "Profile name. Pick the one whose purpose best matches the work.",
							"enum":        profileEnum,
						},
						"prompt": map[string]interface{}{
							"type":        "string",
							"description": "Full self-contained instructions for the subagent. Include all context, constraints, success criteria, and the exact format you want the report in. The subagent has no memory of your conversation.",
						},
					},
					"required": []string{"profile", "prompt"},
				},
			},
		},
		{
			Type: llm.ToolTypeFunction,
			Function: &llm.FunctionDef{
				Name:        "subagent_spawn",
				Description: spawnDesc,
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"profile": map[string]interface{}{
							"type":        "string",
							"description": "Profile name. Pick the one whose purpose best matches the work.",
							"enum":        profileEnum,
						},
						"prompt": map[string]interface{}{
							"type":        "string",
							"description": "Full self-contained instructions for the subagent. Same rules as subagent_run.",
						},
					},
					"required": []string{"profile", "prompt"},
				},
			},
		},
		{
			Type: llm.ToolTypeFunction,
			Function: &llm.FunctionDef{
				Name:        "subagent_wait",
				Description: "Block until a previously-spawned subagent reports, then return its report inline (the same shape subagent_run returns). Use this when you spawned async, did some other work, and now you need the answer to continue. The wait wakes early if an operator message arrives -- in that case the subagent is still running and you can call subagent_wait again, or check subagent_status. The report is drained from your inbox here, so you won't see it again in your normal between-turn drain.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"work_id": map[string]interface{}{
							"type":        "string",
							"description": "The work_id returned by subagent_spawn.",
						},
					},
					"required": []string{"work_id"},
				},
			},
		},
		{
			Type: llm.ToolTypeFunction,
			Function: &llm.FunctionDef{
				Name:        "subagent_status",
				Description: "List currently running subagents (those started with subagent_spawn that haven't reported yet). Returns work_id, profile, elapsed seconds, and a truncated prompt preview for each. Use this if you're not sure whether to wait for a previously-spawned subagent or move on.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: llm.ToolTypeFunction,
			Function: &llm.FunctionDef{
				Name:        "subagent_cancel",
				Description: "Cooperatively cancel a running subagent. The subagent's in-flight LLM call aborts and its loop exits; if it had partial work, that's lost. Use this when you've changed your mind or realized the subagent is on the wrong track. Returns immediately; the cancellation report still lands in your inbox shortly after.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"work_id": map[string]interface{}{
							"type":        "string",
							"description": "The work_id returned by subagent_spawn (e.g. sub-9af3b2c1bb8d).",
						},
					},
					"required": []string{"work_id"},
				},
			},
		},
	}
}

// subagentReportToolDef is the child-only tool. Calling it terminates
// the child's agent loop; main.go's spawnFn closure delivers the
// report text back to the parent's Manager.
func (te *Executor) subagentReportToolDef() llm.Tool {
	return llm.Tool{
		Type: llm.ToolTypeFunction,
		Function: &llm.FunctionDef{
			Name:        "subagent_report",
			Description: "Deliver your final report to the parent agent and exit. The text you provide here is what the parent receives -- everything else from your conversation is discarded. Make it self-contained: state what was asked, what you did, what you found, and any caveats. After this returns, your loop will exit; do not continue with further tool calls.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"summary": map[string]interface{}{
						"type":        "string",
						"description": "The complete, self-contained report. Markdown is fine. The parent will see this verbatim.",
					},
				},
				"required": []string{"summary"},
			},
		},
	}
}

// subagentRun handles the synchronous primary-side dispatch.
func (te *Executor) subagentRun(ctx context.Context, argsJSON string) string {
	if te.subagentMgr == nil {
		return "Error: subagent support is not enabled."
	}
	var args struct {
		Profile string `json:"profile"`
		Prompt  string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if args.Profile == "" {
		return "Error: profile is required."
	}
	if args.Prompt == "" {
		return "Error: prompt is required."
	}
	report, err := te.subagentMgr.Run(ctx, args.Profile, args.Prompt)
	if err != nil {
		return fmt.Sprintf("Subagent run failed: %s", err)
	}
	return formatReport(report)
}

// subagentSpawn handles asynchronous primary-side dispatch.
func (te *Executor) subagentSpawn(ctx context.Context, argsJSON string) string {
	if te.subagentMgr == nil {
		return "Error: subagent support is not enabled."
	}
	var args struct {
		Profile string `json:"profile"`
		Prompt  string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if args.Profile == "" {
		return "Error: profile is required."
	}
	if args.Prompt == "" {
		return "Error: prompt is required."
	}
	workID, err := te.subagentMgr.Spawn(ctx, args.Profile, args.Prompt)
	if err != nil {
		return fmt.Sprintf("Subagent spawn failed: %s", err)
	}
	return fmt.Sprintf("Subagent spawned (profile=%s, work_id=%s). The report will arrive in your inbox.", args.Profile, workID)
}

// subagentWait blocks until the named subagent reports, drains the
// report from the inbox, and returns it inline. Wakes early on
// operator messages (sleep parity) so a long-running subagent can't
// indefinitely hide an incoming collaborator turn.
func (te *Executor) subagentWait(ctx context.Context, argsJSON string) string {
	if te.subagentMgr == nil {
		return "Error: subagent support is not enabled."
	}
	var args struct {
		WorkID string `json:"work_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if args.WorkID == "" {
		return "Error: work_id is required."
	}

	// Run Manager.Wait in a goroutine so the handler can poll the
	// operator queue alongside.
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		rep   subagent.Report
		found bool
		err   error
	}
	done := make(chan result, 1)
	go func() {
		rep, found, err := te.subagentMgr.Wait(waitCtx, args.WorkID)
		done <- result{rep, found, err}
	}()

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case r := <-done:
			if r.err != nil {
				return fmt.Sprintf("subagent_wait: %s", r.err)
			}
			if !r.found {
				return fmt.Sprintf("subagent_wait: no report found for %s", args.WorkID)
			}
			return formatReport(r.rep)
		case <-ctx.Done():
			cancel()
			<-done
			return "subagent_wait: canceled."
		case <-ticker.C:
			if te.telegram != nil && te.telegram.HasPending() {
				cancel()
				<-done
				return fmt.Sprintf("subagent_wait: operator message arrived; subagent %s is still running. Drain the operator message, then call subagent_wait again or subagent_status to check progress.", args.WorkID)
			}
		}
	}
}

// subagentStatus lists active spawned subagents.
func (te *Executor) subagentStatus() string {
	if te.subagentMgr == nil {
		return "Error: subagent support is not enabled."
	}
	active := te.subagentMgr.Status()
	if len(active) == 0 {
		return "No active subagents."
	}
	sort.Slice(active, func(i, j int) bool {
		return active[i].Started.Before(active[j].Started)
	})
	var b strings.Builder
	fmt.Fprintf(&b, "%d active subagent(s):\n", len(active))
	for _, a := range active {
		elapsed := time.Since(a.Started).Round(time.Second)
		kind := "spawn"
		if !a.Async {
			kind = "run"
		}
		fmt.Fprintf(&b, "  - %s [%s, profile=%s, %s elapsed]\n      %s\n",
			a.WorkID, kind, a.Profile, elapsed, a.Prompt)
	}
	return strings.TrimRight(b.String(), "\n")
}

// subagentCancel cancels a single running subagent.
func (te *Executor) subagentCancel(argsJSON string) string {
	if te.subagentMgr == nil {
		return "Error: subagent support is not enabled."
	}
	var args struct {
		WorkID string `json:"work_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if args.WorkID == "" {
		return "Error: work_id is required."
	}
	if err := te.subagentMgr.Cancel(args.WorkID); err != nil {
		return fmt.Sprintf("Cancel failed: %s", err)
	}
	return fmt.Sprintf("Cancellation requested for %s. The cancellation report will arrive in your inbox shortly.", args.WorkID)
}

// subagentReport is the child-only handler. It calls the report sink
// supplied by main.go's spawnFn, then returns a confirmation. The
// spawnFn is responsible for stopping the child agent loop after
// the sink fires; this handler does not block.
func (te *Executor) subagentReport(argsJSON string) string {
	if te.subagentReportFn == nil {
		return "Error: subagent_report is only available when running as a subagent."
	}
	var args struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}
	if args.Summary == "" {
		return "Error: summary is required."
	}
	te.subagentReportFn(args.Summary)
	return "Report delivered. The subagent loop will exit on the next iteration."
}

// formatReport turns a Report into the string the primary's
// subagent_run tool returns. Includes status flags so the primary
// can react appropriately to truncation / cancellation.
func formatReport(r subagent.Report) string {
	var b strings.Builder
	if r.Truncated {
		b.WriteString("[truncated -- subagent hit turn or time cap before reporting]\n\n")
	}
	if r.Canceled {
		b.WriteString("[canceled -- subagent was canceled before reporting]\n\n")
	}
	if r.Err != nil {
		fmt.Fprintf(&b, "[error: %s]\n\n", r.Err)
	}
	if r.Text == "" {
		b.WriteString("(no report content)")
	} else {
		b.WriteString(r.Text)
	}
	return b.String()
}
