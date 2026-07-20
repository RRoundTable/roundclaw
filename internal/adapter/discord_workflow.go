package adapter

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

func (d *Discord) handleWorkflow(i *discordgo.InteractionCreate, agentID string) {
	d.defer_(i)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := d.disp.Workflow(ctx, agentID)
	if err != nil {
		d.followUp(i, "⚠️ "+err.Error())
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**%s** — workflow `%s`\n", info.AgentID, info.WorkflowID)

	if info.Unavailable != "" {
		// Usually means the agent has never run, which is not a fault.
		b.WriteString("_No Temporal execution._ " + info.Unavailable)
		d.followUp(i, b.String())
		return
	}

	fmt.Fprintf(&b, "status `%s` · %d queued · %d turns · history %d\n",
		info.Status, info.QueueLength, info.TurnCount, info.HistoryLength)
	if info.StartedAt != "" {
		fmt.Fprintf(&b, "started %s\n", info.StartedAt)
	}

	if info.ActivityType != "" {
		fmt.Fprintf(&b, "\nrunning `%s` (%s)", info.ActivityType, info.ActivityState)
		if info.ActivityAttempt > 1 {
			// The number that explains a "stuck" agent: the transcript looks
			// idle while the activity keeps failing and being retried.
			fmt.Fprintf(&b, " — **attempt %d**, it is failing and retrying", info.ActivityAttempt)
		}
		b.WriteString("\n")
		if info.LastFailure != "" {
			fmt.Fprintf(&b, "last error: `%s`\n", truncate(info.LastFailure, 300))
		}
	}
	d.followUp(i, b.String())
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
