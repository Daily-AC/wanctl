package protocol

// A workspace envelope cannot be mistaken for a legacy command by an old
// agent. In particular, sending a new optional field on file_read would let
// old agents silently resolve its path outside the intended workspace.
const KindWorkspace = "workspace"

// WorkspaceResult describes live device state, not a serialized shell that
// can be restored after a device restart. Code is meaningful only when Done.
type WorkspaceResult struct {
	ID              string `json:"id"`
	Root            string `json:"root"`
	State           string `json:"state"`
	RequestID       string `json:"request_id,omitempty"`
	RequestState    string `json:"request_state,omitempty"`
	ActiveRequestID string `json:"active_request_id,omitempty"`
	Done            bool   `json:"done"`
	Code            int    `json:"code"`
	Error           string `json:"error,omitempty"`
	Output          string `json:"output,omitempty"`
	NextOffset      int64  `json:"next_offset"`
	OutputBytes     int64  `json:"output_bytes"`
	RetainedBytes   int64  `json:"retained_bytes"`
	Truncated       bool   `json:"truncated"`
}
