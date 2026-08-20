package serve

import (
	"reasonix/internal/event"
	"reasonix/internal/eventwire"
)

// Keep the serve package's historical local type names as aliases, but delegate
// the actual JSON contract and domain-event mapping to the shared eventwire package.
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
