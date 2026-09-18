package server

// Cancelling a command in a persistent session is not the same problem as
// cancelling a one-shot. A one-shot owns its shell, so the cancel hook in
// command_cancel_{unix,windows}.go kills the whole tree, shell included. A
// session's shell owns the working directory and the environment that later
// connections rely on, so it has to survive: only the command running in the
// foreground may be killed (issue #46, option (a)).
//
// The agent never holds a handle to that command. The session runs commands by
// writing them to the shell's stdin, so the shell — not the agent — forks them.
// What the agent does hold is the shell's pid, and every process the shell
// started for the command it is running now is a descendant of it. So the
// mechanism is: snapshot the process table, take the subtree below the session
// shell, and kill it from the leaves up, leaving the shell itself alone. The
// shell's wait then returns, it prints the end-of-command marker it was already
// given, and the session is ready for the next command with its cwd intact.
//
// The per-OS pieces are the snapshot and the kill; the subtree walk below is
// shared and testable anywhere.
//
// Two limits are inherent to this and are not bugs to be worked around:
//
//   - A command that forks nothing has nothing to kill. `while :; do :; done`
//     in the session shell, or a PowerShell cmdlet such as Start-Sleep (which
//     runs inside powershell.exe rather than as a child), cannot be cancelled
//     without destroying the session. Use -oneshot for anything like that.
//   - A process the session deliberately left in the background is a
//     descendant too, so it goes with the foreground command. That is what
//     closing a terminal does; long-running work belongs in `exec-async`,
//     which is detached from any session on purpose.

import "sort"

// descendants returns every process below root in the parent map, ordered
// deepest first so a child is signalled before the parent that might otherwise
// notice it dying and react. root itself is never included.
func descendants(parents map[int]int, root int) []int {
	children := map[int][]int{}
	for pid, ppid := range parents {
		if pid == ppid || pid <= 0 {
			continue // a self-parented or bogus row cannot start a cycle
		}
		children[ppid] = append(children[ppid], pid)
	}
	var out []int
	depth := map[int]int{root: 0}
	seen := map[int]bool{root: true}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		kids := children[pid]
		sort.Ints(kids)
		for _, kid := range kids {
			if seen[kid] {
				continue // defends against a cycle in a torn snapshot
			}
			seen[kid] = true
			depth[kid] = depth[pid] + 1
			out = append(out, kid)
			queue = append(queue, kid)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return depth[out[i]] > depth[out[j]] })
	return out
}

// parseProcessTable reads "pid ppid" rows, one process per line, and tolerates a
// header line and leading whitespace. `ps` output differs between macOS, procps
// and toybox in its header and padding but not in those first two columns.
func parseProcessTable(table string) map[int]int {
	parents := map[int]int{}
	for line := range splitLines(table) {
		pid, ppid, ok := twoInts(line)
		if !ok {
			continue // header, blank line, or a row we cannot read
		}
		parents[pid] = ppid
	}
	return parents
}

// killProcessTree ends every descendant of shellPID, deepest first, and leaves
// shellPID itself running. Processes that vanished between the snapshot and the
// signal are not errors; they are the outcome asked for.
func killProcessTree(shellPID int) error {
	parents, err := processParents()
	if err != nil {
		return err
	}
	victims := descendants(parents, shellPID)
	if len(victims) == 0 {
		return nil
	}
	var firstErr error
	for _, pid := range victims {
		if err := terminateProcess(pid); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
