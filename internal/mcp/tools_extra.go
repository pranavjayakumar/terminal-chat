package mcp

// The agent's helper tools: catching up, the scheduler, and asking another
// agent. The supervisor gates each by who asked (internal/agent/extras.go).
const (
	ToolCatchUp        = "catch_up"
	ToolSchedule       = "schedule"
	ToolRemind         = "remind"
	ToolListSchedules  = "list_schedules"
	ToolCancelSchedule = "cancel_schedule"
	ToolAskAgent       = "ask_agent"
)

const whenDesc = `when, in your owner's local time: "in 20m", "in 2h", "17:30", "5pm", "tomorrow 9am", "friday 10:00" or "2026-10-25 09:00"`

var extraAgentTools = []Tool{
	{ToolCatchUp, "What the asker missed: recent messages (and active threads) in the channels this ask may read, to summarize for them. For \"what did I miss?\".", schema(
		prop{"chan", "string", "only this channel (optional; default: every channel this ask may read)", false},
		prop{"since", "string", "how far back: \"2h\", \"3d\", \"today\", \"yesterday\", \"week\" or a date (default: your owner's unread, else 24h)", false})},
	{ToolSchedule, "Your owner only: post a message later, once or on repeat. You post it yourself, as you.", schema(
		prop{"when", "string", whenDesc, true},
		prop{"chan", "string", chanDesc + ` (default: here)`, false},
		prop{"text", "string", "the message; @names must be real tc names (they are checked)", true},
		prop{"repeat", "string", "once (default), daily or weekdays", false})},
	{ToolRemind, "Remind someone of something later: you post \"⏰ @person …\". Anyone may ask for a reminder for themselves; only your owner may set one for someone else.", schema(
		prop{"person", "string", "who: \"me\" (the asker) or their tc name", true},
		prop{"when", "string", whenDesc, true},
		prop{"text", "string", "what to remind them of", true})},
	{ToolListSchedules, "List pending schedules and reminders (your owner sees all; others see their own).", schema()},
	{ToolCancelSchedule, "Cancel a schedule or reminder by id (from list_schedules).", schema(
		prop{"id", "string", "the id, e.g. s4kq7", true})},
	{ToolAskAgent, "Ask another person's agent a question: posts \"@handle question\" where it can see it and waits up to 5 minutes for its reply, which this returns.", schema(
		prop{"handle", "string", "the other agent's handle, without @", true},
		prop{"question", "string", "the question, self-contained", true},
		prop{"chan", "string", "channel to ask in (default: this thread; required when you're in a DM, which other agents can't see)", false})},
}

func init() {
	for _, t := range extraAgentTools {
		allTools[t.Name] = t
	}
}

func extraAgentToolNames() []string {
	out := make([]string, len(extraAgentTools))
	for i, t := range extraAgentTools {
		out[i] = t.Name
	}
	return out
}
