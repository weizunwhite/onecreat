package main

import (
	"reasonix/internal/event"
	"reasonix/internal/eventwire"
)

// Desktop and HTTP/SSE must expose the same event contract. Keep these aliases so
// the rest of the desktop package (and its tests) does not know where the wire
// schema lives, while all actual encoding is owned by internal/eventwire.
type wireEvent = eventwire.Event
type wireCompaction = eventwire.Compaction
type wireAskOption = eventwire.AskOption
type wireAskQuestion = eventwire.AskQuestion
type wireAsk = eventwire.Ask
type wireTool = eventwire.Tool
type wireUsage = eventwire.Usage
type wireApproval = eventwire.Approval

var kindNames = eventwire.KindNames

func toWireAsk(a event.Ask) *wireAsk { return eventwire.EncodeAsk(a) }

func toWire(e event.Event) wireEvent { return eventwire.Encode(e) }
