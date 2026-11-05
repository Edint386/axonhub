package bailian

import (
	"encoding/json"

	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/server/realtime/dialect"
)

// Billing of the qwen-audio models on the inference WebSocket. Every billable unit is an
// asrUsage: tokens and seconds for -message, seconds only for -streaming. Paraformer keeps its
// own per-final duration billing in paraformer.go.

// qwenAudioFinalUsage bills one final: the GA usage object of its completed event and its
// usage records.
func (d *ParaformerDialect) qwenAudioFinalUsage(ev, sent gjson.Result, id, text string, endMs int64) (map[string]any, []dialect.UsageRecord) {
	var u asrUsage

	switch d.prof.usage {
	case usageReportedTokens:
		u = d.meter.next(parseASRUsage(reportedUsage(ev)))
	case usageAudioSeconds:
		u = asrUsage{Duration: d.billAudioTo(d.offsetTicks + endMs*paraformerTicksPerMs)}
	}

	summary := map[string]any{
		"type":             d.prof.summaryType,
		"item_id":          id,
		"transcript":       text,
		"duration_seconds": u.Duration,
		"begin_time_ms":    d.sessionMs(sent.Get("begin_time").Int()),
		"end_time_ms":      d.sessionMs(endMs),
	}

	return d.gaUsage(u), d.usageRecords(u, id, summary)
}

// taskTail bills what a finished task used beyond its finals: the rest of the reported usage
// (task-finished carries the task's final total) or the audio forwarded after the last final.
// Paraformer bills its finals only.
func (d *ParaformerDialect) taskTail(ev gjson.Result) asrUsage {
	switch d.prof.usage {
	case usageReportedTokens:
		// Counters cannot restart within a task: a report below the finals' (say, duration
		// only) is not the meter's restart case and bills nothing.
		cur, prev := parseASRUsage(reportedUsage(ev)), d.meter.last
		if cur.Total < prev.Total || cur.Input < prev.Input || cur.Output < prev.Output || cur.Duration < prev.Duration {
			return asrUsage{}
		}

		return d.meter.next(cur)
	case usageAudioSeconds:
		return asrUsage{Duration: d.billAudioTo(d.forwardedTicks)}
	default:
		return asrUsage{}
	}
}

// tailRecords records a task tail against the item it belongs to: the last committed one (a
// commit reply synthesised for this task, or its last final), else the task itself.
func (d *ParaformerDialect) tailRecords(u asrUsage) []dialect.UsageRecord {
	id := d.lastCommitted
	if id == "" {
		id = "task_" + d.taskID
	}

	return d.usageRecords(u, id, map[string]any{
		"type":             "qwen_audio_task_tail",
		"item_id":          id,
		"task_id":          d.taskID,
		"duration_seconds": u.Duration,
	})
}

// billAudioTo bills the session audio up to sessionTicks (clamped to the audio forwarded
// upstream, which never includes audio the session dropped) and returns the whole seconds not
// billed before. Billing whole seconds rounded up on the session timeline keeps the total at
// most one second over the audio forwarded, however many finals, commits, tasks and endings it
// is split into, and never counts a stretch twice.
func (d *ParaformerDialect) billAudioTo(sessionTicks int64) int64 {
	const ticksPerSecond = 1000 * paraformerTicksPerMs

	sessionTicks = max(min(sessionTicks, d.forwardedTicks), 0)
	target := (sessionTicks + ticksPerSecond - 1) / ticksPerSecond
	billed := max(target-d.billedSeconds, 0)
	d.billedSeconds = max(d.billedSeconds, target)

	return billed
}

// gaUsage renders u as the usage object of a GA transcription completed event.
func (d *ParaformerDialect) gaUsage(u asrUsage) map[string]any {
	if d.prof.usage == usageReportedTokens {
		return u.gaTokens()
	}

	return map[string]any{"type": "duration", "seconds": u.Duration}
}

// usageRecords returns the record of u for item id, or nothing when u bills nothing.
func (d *ParaformerDialect) usageRecords(u asrUsage, id string, summary map[string]any) []dialect.UsageRecord {
	if u.Duration == 0 && u.Total == 0 && u.Input == 0 && u.Output == 0 {
		return nil
	}

	if d.prof.usage == usageReportedTokens {
		summary["input_tokens"] = u.Input
		summary["output_tokens"] = u.Output
		summary["total_tokens"] = u.Total
	}

	b, _ := json.Marshal(summary)

	return []dialect.UsageRecord{u.record(id, d.opts.ActualModel, b)}
}

// reportedUsage is the usage object of an inference event: payload.usage, which the docs and
// the live probe show, or its payload.output.usage echo.
func reportedUsage(ev gjson.Result) gjson.Result {
	if u := ev.Get("payload.usage"); u.IsObject() {
		return u
	}

	return ev.Get("payload.output.usage")
}
