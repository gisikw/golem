package herdr

import "github.com/gisikw/golem/protocol"

// MapStatus projects a Herdr agent status onto a Golem observation, exactly as
// docs/herdr-rebase.md §3.2 specifies. Golem's ledger stays authoritative;
// these are observations, never settlements.
//
//	working -> running
//	idle    -> running   ("ready for input", not "finished")
//	done    -> running   ("idle after unseen work"; it never settles a job)
//	blocked -> blocked
//	unknown -> keep the last known state, flagged stale
//
// known is false for any status this client does not recognise, which is also
// treated as stale rather than as evidence of anything.
func MapStatus(status string) (state protocol.State, stale bool, known bool) {
	switch status {
	case "working", "idle", "done":
		return protocol.Running, false, true
	case "blocked":
		return protocol.Blocked, false, true
	case "unknown":
		return "", true, true
	default:
		return "", true, false
	}
}
