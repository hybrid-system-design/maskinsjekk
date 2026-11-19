package state

import (
	"maskinsjekk/internal/config"
	"maskinsjekk/internal/store"
)

// Job is one stretch of work: consecutive HEATING/RUNNING states. End is 0
// while the job is still going.
type Job struct {
	Start int64
	End   int64
}

// busy reports whether a state is part of a job.
func busy(state string) bool { return state == config.StateHeating || state == config.StateRunning }

// Jobs derives jobs from state events (sorted by start). A gap of UNKNOWN
// (missing data) between busy states does not split a job; OFF or IDLE ends
// it. Jobs are derived on read rather than stored so a recompute of the
// state history updates them too.
func Jobs(evs []store.StateEvent) []Job {
	var out []Job
	var cur *Job
	var lastBusyEnd int64 // end of the latest busy event in cur (0 = open)
	for _, ev := range evs {
		switch {
		case busy(ev.State):
			if cur == nil {
				out = append(out, Job{Start: ev.Start})
				cur = &out[len(out)-1]
			}
			lastBusyEnd = ev.End
			cur.End = ev.End
		case ev.State == config.StateUnknown:
			// Keep the job open across a data gap; if it ends here, it ended
			// when the last busy state did.
		default:
			if cur != nil {
				cur.End = lastBusyEnd
				cur = nil
			}
		}
	}
	return out
}
