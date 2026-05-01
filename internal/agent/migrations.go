package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/matjam/faultline/internal/llm"
	prompt "github.com/matjam/faultline/internal/prompts"
	skillsdomain "github.com/matjam/faultline/internal/skills"
	"github.com/matjam/faultline/internal/subagent"
)

// Prompt-migration runner. Called once at startup, between
// initializeContext and the main Run loop. Each pending migration is
// applied in its own short bounded sub-loop using the agent's normal
// ChatModel + Tools surface; the migration body tells the LLM what to
// edit and the LLM uses memory_edit / memory_insert / memory_write
// against its own prompt files.
//
// Why this is a separate phase rather than just appending the
// instructions to the resumed conversation:
//
//   - Resumed conversations may be deep into some unrelated work and
//     pushing migration instructions into them muddies the model's
//     context and risks polluting downstream replies.
//   - Migrations should be applied against an ephemeral context that
//     is discarded once recording completes, so the long-running
//     conversation log on disk stays clean.
//
// Failure semantics:
//
//   - LLM error mid-migration: log it, record the migration as
//     applied with a "failed" note (so it does not block subsequent
//     migrations or future startups), continue.
//   - Hard turn cap reached without a clean text-only response: same
//     treatment — record with a "turn-cap" note. Operator can delete
//     the line manually to retry.
//   - Tool errors during the LLM's application steps are visible to
//     the LLM (it sees the tool result body) and it can retry within
//     the same migration's turn budget.
//
// We deliberately do not retry across startups by default: the agent
// loop must always make forward progress, and a buggy migration that
// fails on every retry would otherwise wedge a deployment.

// migrationsTurnCap bounds the per-migration LLM/tool loop. The
// expected migration takes 2-4 turns (read, edit, verify, confirm);
// 12 leaves comfortable headroom for retries and corrections without
// allowing a runaway.
const migrationsTurnCap = 12

// runPromptMigrations applies any embedded migrations not yet recorded
// in prompts/migrations.md.
//
// It returns the (possibly rebuilt) messages and prompts maps. When
// migrations were applied the system message is rebuilt from disk and
// any in-flight resumed conversation history is preserved. When no
// migrations are pending the inputs are returned unchanged.
//
// Subagents skip migrations entirely: the operator's prompts are not
// theirs to mutate, and a child running through migrations would
// duplicate the parent's work and pollute the child's report scope.
func (a *Agent) runPromptMigrations(
	ctx context.Context,
	toolCtx context.Context,
	messages []llm.Message,
	toolDefs []llm.Tool,
	prompts map[string]string,
) ([]llm.Message, map[string]string, error) {
	// Children inherit a system prompt override and never own the
	// prompt files; skip without touching the log.
	if a.systemPromptOverride != "" {
		return messages, prompts, nil
	}

	all, err := prompt.LoadMigrations()
	if err != nil {
		return messages, prompts, fmt.Errorf("load embedded migrations: %w", err)
	}
	if len(all) == 0 {
		return messages, prompts, nil
	}

	applied, err := prompt.LoadAppliedMigrations(a.memory)
	if err != nil {
		return messages, prompts, fmt.Errorf("load applied migrations: %w", err)
	}

	pending := prompt.PendingMigrations(all, applied)
	if len(pending) == 0 {
		return messages, prompts, nil
	}

	a.logger.Info("prompt migrations: applying pending",
		"pending", len(pending),
		"total_shipped", len(all),
		"already_applied", len(applied))

	// Apply each migration in its own ephemeral conversation. We
	// discard the per-migration context after recording so the
	// resumed (or fresh) primary conversation history we go on to
	// use is unaffected.
	for _, m := range pending {
		note := a.applyOneMigration(ctx, toolCtx, toolDefs, prompts, m)
		recordErr := prompt.RecordMigrationApplied(a.memory, m, time.Now(), note)
		if recordErr != nil {
			// If we cannot persist the record, retrying on next
			// startup would re-apply the migration. That is the
			// right safe failure mode (idempotent migrations
			// tolerate it), but log loudly.
			a.logger.Error("prompt migrations: record write failed",
				"id", m.ID, "slug", m.Slug, "error", recordErr)
		} else {
			a.logger.Info("prompt migrations: applied",
				"id", m.ID, "slug", m.Slug, "note", note)
		}
	}

	// Reload prompts and rebuild messages[0] in case migrations
	// edited system.md or any other prompt file. Preserve the rest
	// of the conversation so a resumed session continues where it
	// left off, just with the updated system prompt at index 0.
	newPrompts, err := prompt.LoadAll(a.memory)
	if err != nil {
		return messages, prompts, fmt.Errorf("reload prompts after migrations: %w", err)
	}

	newSystemMsg := a.buildFreshSystemMessage(newPrompts)
	if len(messages) > 0 && messages[0].Role == llm.RoleSystem {
		messages[0] = newSystemMsg
	} else {
		// Defensive: should not happen because initializeContext
		// always produces a system message at index 0, but
		// preserve safety.
		messages = append([]llm.Message{newSystemMsg}, messages...)
	}

	return messages, newPrompts, nil
}

// applyOneMigration runs the bounded sub-loop for a single migration
// and returns the recorded "note" suffix (empty string on a clean
// completion, "error: ..." on a failure path).
//
// The sub-loop builds its own ephemeral message log: a freshly-built
// system message + a single user message containing the migration's
// title and body. ctx is the parent context (used for Chat calls so
// in-flight generations can finish on graceful shutdown). toolCtx is
// the shutdown-aware context used for Tools.Execute so any
// long-running tool yields promptly to a shutdown.
func (a *Agent) applyOneMigration(
	ctx context.Context,
	toolCtx context.Context,
	toolDefs []llm.Tool,
	prompts map[string]string,
	m prompt.Migration,
) string {
	a.logger.Info("prompt migrations: starting", "id", m.ID, "slug", m.Slug)

	systemMsg := a.buildFreshSystemMessage(prompts)
	userPrompt := buildMigrationUserPrompt(m)

	msgs := []llm.Message{
		systemMsg,
		{Role: llm.RoleUser, Content: userPrompt},
	}

	for turn := 0; turn < migrationsTurnCap; turn++ {
		// Honor shutdown / cancellation.
		select {
		case <-ctx.Done():
			return "interrupted: shutdown"
		default:
		}

		resp, err := a.chat.Chat(ctx, a.chatReq(msgs, toolDefs))
		if err != nil {
			if ctx.Err() != nil {
				return "interrupted: shutdown"
			}
			a.logger.Error("prompt migrations: LLM call failed",
				"id", m.ID, "turn", turn, "error", err)
			return fmt.Sprintf("error: llm-call (%s)", truncateForNote(err.Error()))
		}
		if len(resp.Choices) == 0 {
			return "error: empty-response"
		}

		choice := resp.Choices[0].Message
		msgs = append(msgs, choice)
		if choice.Content != "" {
			a.logThought(choice.Content)
		}

		if len(choice.ToolCalls) == 0 {
			// Text-only response signals "done".
			return ""
		}

		// Execute the tool calls. SetContextInfo gives the tool
		// layer the same token-count signal it gets in the main
		// loop so context_status etc. behave consistently.
		a.tools.SetContextInfo(a.countMessageTokens(msgs))
		for _, tc := range choice.ToolCalls {
			result := a.tools.Execute(toolCtx, tc)
			msgs = append(msgs, toolMessage(tc.ID, result))
		}
	}

	a.logger.Warn("prompt migrations: turn cap reached without clean completion",
		"id", m.ID, "slug", m.Slug, "cap", migrationsTurnCap)
	return fmt.Sprintf("error: turn-cap-%d", migrationsTurnCap)
}

// buildFreshSystemMessage constructs a system message reflecting the
// current state on disk: prompts as just loaded, plus the standard
// recent-memory / skill / subagent catalog scaffolding from
// BuildCycleContext.
//
// Used both at the start of each migration's sub-loop (so the LLM
// applying the migration sees an up-to-date view of itself) and after
// all migrations have run (to refresh the primary conversation's
// system message in place).
func (a *Agent) buildFreshSystemMessage(prompts map[string]string) llm.Message {
	memories := a.gatherContextMemories()
	var skillCatalog []skillsdomain.Skill
	if a.skills != nil {
		skillCatalog = a.skills.List()
	}
	var subagentCatalog []subagent.Catalog
	if a.subagents != nil {
		profiles := a.subagents.Profiles()
		subagentCatalog = make([]subagent.Catalog, 0, len(profiles))
		for _, p := range profiles {
			subagentCatalog = append(subagentCatalog, p.ToCatalog())
		}
	}
	body := prompt.BuildCycleContext(
		prompts["system"], memories, skillCatalog, subagentCatalog,
		time.Now(), a.cfg.Limits.RecentMemoryChars,
	)
	return llm.Message{Role: llm.RoleSystem, Content: body}
}

// buildMigrationUserPrompt frames the migration body as a user-role
// instruction. The framing tells the LLM what to do with the body
// (apply, then reply text-only) and reminds it that the runtime
// records completion automatically.
func buildMigrationUserPrompt(m prompt.Migration) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[Prompt migration %03d — %s]\n\n", m.ID, m.Slug)
	sb.WriteString(
		"You are being asked to apply a one-time prompt update shipped " +
			"with this faultline release. Read the instructions below carefully, " +
			"perform the requested edits using your normal memory tools, and " +
			"verify the result. When you are done, reply with a single short " +
			"text-only message describing what you did — that signals completion " +
			"and the runtime will record it. Do not call further tools after " +
			"that final text reply.\n\n",
	)
	sb.WriteString("--- migration body ---\n\n")
	sb.WriteString(m.Body)
	if !strings.HasSuffix(m.Body, "\n") {
		sb.WriteString("\n")
	}
	sb.WriteString("--- end migration body ---\n")
	return sb.String()
}

// truncateForNote keeps a recorded error note short. Notes go on a
// single line in prompts/migrations.md; multi-line stack traces would
// break the line-based parser.
func truncateForNote(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	const max = 80
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
