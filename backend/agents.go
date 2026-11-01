package main

import "context"

type agentKey struct{}

type agentChannel struct {
	name string
	send func(map[string]any)
}

// withAgent ties work to one robot on the page, so anything using this context, including the
// retry loop in fetch, reports what really happened to that robot.
func withAgent(ctx context.Context, name string, send func(map[string]any)) context.Context {
	return context.WithValue(ctx, agentKey{}, agentChannel{name, send})
}

// report sends a live activity line. total of 0 means there's no measurable progress to show.
func report(ctx context.Context, message string, done, total int) {
	if channel, ok := ctx.Value(agentKey{}).(agentChannel); ok {
		channel.send(map[string]any{"stage": "agent", "agent": channel.name, "message": message, "done": done, "total": total})
	}
}

func reportRetry(ctx context.Context, message string) {
	if channel, ok := ctx.Value(agentKey{}).(agentChannel); ok {
		channel.send(map[string]any{"stage": "agent", "agent": channel.name, "message": message, "retry": true})
	}
}
