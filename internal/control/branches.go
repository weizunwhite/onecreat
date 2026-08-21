package control

import (
	"fmt"
	"log/slog"
	"reasonix/internal/event"
	"reasonix/internal/provider"
	"strconv"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/engine"
)

// ParseBranchTarget parses the arguments after "/branch". A leading positive
// integer means "branch from displayed turn N"; otherwise the whole argument is
// the optional branch name for a tip branch.
func ParseBranchTarget(args string) (turn int, name string, fromTurn bool, err error) {
	args = strings.TrimSpace(args)
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return 0, "", false, nil
	}
	n, convErr := strconv.Atoi(fields[0])
	if convErr != nil {
		return 0, args, false, nil
	}
	if n <= 0 {
		return 0, "", false, fmt.Errorf("usage: /branch [turn] [name]")
	}
	name = strings.TrimSpace(strings.TrimPrefix(args, fields[0]))
	return n, name, true, nil
}

func (c *Controller) BranchTreeText() string {
	branches, err := c.Branches()
	if err != nil {
		return "branches: " + err.Error()
	}
	return FormatBranchTree(branches, agent.BranchID(c.session.Path()))
}

func FormatBranchTree(branches []agent.BranchInfo, currentID string) string {
	if len(branches) == 0 {
		return "branches: none"
	}
	byID := map[string]agent.BranchInfo{}
	children := map[string][]agent.BranchInfo{}
	for _, b := range branches {
		byID[b.ID] = b
	}
	var roots []agent.BranchInfo
	for _, b := range branches {
		if b.ParentID == "" {
			roots = append(roots, b)
			continue
		}
		if _, ok := byID[b.ParentID]; !ok {
			roots = append(roots, b)
			continue
		}
		children[b.ParentID] = append(children[b.ParentID], b)
	}
	var out strings.Builder
	out.WriteString("branches:\n")
	seen := map[string]bool{}
	var walk func(agent.BranchInfo, string, bool, int)
	walk = func(b agent.BranchInfo, prefix string, last bool, depth int) {
		if seen[b.ID] {
			return
		}
		seen[b.ID] = true
		joint := "├─"
		childPrefix := prefix + "│  "
		if last {
			joint = "└─"
			childPrefix = prefix + "   "
		}
		current := ""
		if b.ID == currentID {
			current = "  current"
		}
		fmt.Fprintf(&out, "%s%s %s  %s  %s%s\n",
			prefix, joint, shortBranchID(b.ID), branchTitle(b, depth), turnText(b.Turns), current)
		for i, child := range children[b.ID] {
			walk(child, childPrefix, i == len(children[b.ID])-1, depth+1)
		}
	}
	for i, root := range roots {
		walk(root, "", i == len(roots)-1, 0)
	}
	for _, b := range branches {
		walk(b, "", true, 0)
	}
	return strings.TrimRight(out.String(), "\n")
}

func branchTitle(b agent.BranchInfo, depth int) string {
	title := strings.TrimSpace(b.Name)
	if title == "" {
		title = strings.TrimSpace(b.Preview)
	}
	if label, ok := structuredBranchLabel(title); ok {
		return label
	}
	maxRunes := 32 - depth*4
	if maxRunes < 18 {
		maxRunes = 18
	}
	title = oneLineBranch(title, maxRunes)
	if title == "" {
		return "(untitled)"
	}
	return title
}

func structuredBranchLabel(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	switch s[0] {
	case '{':
		lower := strings.ToLower(s)
		switch {
		case strings.Contains(lower, `"msg"`) && strings.Contains(lower, "success"):
			return "JSON response: success", true
		case strings.Contains(lower, `"error"`) || strings.Contains(lower, `"errors"`):
			return "JSON payload: error", true
		default:
			return "JSON object", true
		}
	case '[':
		return "JSON array", true
	default:
		return "", false
	}
}

func turnText(n int) string {
	if n == 1 {
		return "1 turn"
	}
	return fmt.Sprintf("%d turns", n)
}

func shortBranchID(id string) string {
	if len(id) >= 16 && numeric(id[:8]) && id[8] == '-' && numeric(id[9:15]) && id[15] == '.' {
		fracEnd := 16
		for fracEnd < len(id) && fracEnd < 19 && id[fracEnd] >= '0' && id[fracEnd] <= '9' {
			fracEnd++
		}
		if fracEnd > 16 {
			return id[4:8] + "-" + id[9:15] + "." + id[16:fracEnd]
		}
		return id[4:8] + "-" + id[9:15]
	}
	return oneLineBranch(id, 18)
}

func numeric(s string) bool {
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return s != ""
}

func oneLineBranch(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	if maxRunes <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	if maxRunes <= 1 {
		return string(r[:maxRunes])
	}
	return string(r[:maxRunes-1]) + "..."
}

// Fork branches the conversation at the start of turn into a NEW session file,
// preserving the current one as the branch point, and switches to the branch. Code
// is untouched (it's a conversation operation). Like a conversation rewind it needs
// the live boundary, so it is unavailable for resumed-session turns and refused
// while a turn runs. Returns the new session path.
func (c *Controller) Fork(turn int) (string, error) {
	return c.ForkNamed(turn, "")
}

func (c *Controller) ForkNamed(turn int, name string) (string, error) {
	if err := c.requireCap("分叉会话", engine.CapFork); err != nil {
		return "", c.rewindFail(err)
	}
	if c.executor == nil {
		return "", c.rewindFail(fmt.Errorf("checkpoints unavailable"))
	}
	if c.session.dir == "" {
		return "", c.rewindFail(fmt.Errorf("fork needs session persistence, which is disabled"))
	}
	if !c.turn.TryBeginExclusive() {
		return "", c.rewindFail(fmt.Errorf("cannot fork while another operation is running"))
	}
	defer c.turn.EndExclusive()
	boundary, hasBound := c.ckpt.Bound(turn)
	if !hasBound {
		return "", c.rewindFail(fmt.Errorf("fork unavailable for turn %d (resumed session)", turn))
	}

	// Persist the current conversation first so the branch point survives, then
	// seed a fresh session with the messages up to the fork and switch to it.
	if err := c.session.Save(); err != nil {
		slog.Warn("controller: pre-fork snapshot", "err", err)
	}
	parentPath := c.session.Path()
	parentID := agent.BranchID(parentPath)
	src := c.executor.Session().Snapshot()
	if boundary > len(src) {
		boundary = len(src)
	}
	forked := append([]provider.Message(nil), src[:boundary]...)
	sess := agent.NewSession("")
	sess.Messages = forked

	newPath := agent.NewSessionPath(c.session.dir, c.label)
	if err := sess.Save(newPath); err != nil {
		return "", c.rewindFail(err)
	}
	if err := agent.SaveBranchMeta(newPath, agent.BranchMeta{
		Name:             strings.TrimSpace(name),
		ParentID:         parentID,
		ForkTurn:         turn,
		ForkMessageIndex: boundary,
	}); err != nil {
		return "", c.rewindFail(err)
	}
	c.executor.SetSession(sess)
	c.session.SetPath(newPath)
	c.ckpt.Rebind(newPath)
	c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo,
		Text: fmt.Sprintf("forked conversation at turn %d into a new session", turn)})
	return newPath, nil
}

// Branch copies the current conversation into a child branch and switches to it.
// Unlike Fork, it branches at the current tip and does not require a checkpoint.
func (c *Controller) Branch(name string) (string, error) {
	if err := c.requireCap("新建分支", engine.CapFork); err != nil {
		return "", c.rewindFail(err)
	}
	if c.executor == nil {
		return "", c.rewindFail(fmt.Errorf("branch unavailable"))
	}
	if c.session.dir == "" {
		return "", c.rewindFail(fmt.Errorf("branch needs session persistence, which is disabled"))
	}
	if !c.turn.TryBeginExclusive() {
		return "", c.rewindFail(fmt.Errorf("cannot branch while another operation is running"))
	}
	defer c.turn.EndExclusive()
	if !c.executor.Session().HasContent() {
		return "", c.rewindFail(fmt.Errorf("nothing to branch yet"))
	}
	if err := c.session.Save(); err != nil {
		return "", c.rewindFail(err)
	}
	parentPath := c.session.Path()
	parentID := agent.BranchID(parentPath)
	src := c.executor.Session().Snapshot()
	branched := append([]provider.Message(nil), src...)
	sess := agent.NewSession("")
	sess.Messages = branched

	newPath := agent.NewSessionPath(c.session.dir, c.label)
	if err := sess.Save(newPath); err != nil {
		return "", c.rewindFail(err)
	}
	if err := agent.SaveBranchMeta(newPath, agent.BranchMeta{
		Name:             strings.TrimSpace(name),
		ParentID:         parentID,
		ForkTurn:         -1,
		ForkMessageIndex: len(branched),
	}); err != nil {
		return "", c.rewindFail(err)
	}
	c.executor.SetSession(sess)
	c.session.SetPath(newPath)
	c.ckpt.Rebind(newPath)
	c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo,
		Text: fmt.Sprintf("created branch %s", agent.BranchID(newPath))})
	return newPath, nil
}

// Branches lists saved conversation branches in this controller's session dir.
func (c *Controller) Branches() ([]agent.BranchInfo, error) {
	if c.session.dir == "" {
		return nil, fmt.Errorf("session persistence is disabled")
	}
	if err := c.session.Save(); err != nil {
		return nil, err
	}
	return agent.ListBranches(c.session.dir)
}

func (c *Controller) SwitchBranch(ref string) (agent.BranchInfo, error) {
	if err := c.requireCap("切换分支", engine.CapFork); err != nil {
		return agent.BranchInfo{}, c.rewindFail(err)
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return agent.BranchInfo{}, c.rewindFail(fmt.Errorf("usage: /switch <branch id|name>"))
	}
	if !c.turn.TryBeginExclusive() {
		return agent.BranchInfo{}, c.rewindFail(fmt.Errorf("cannot switch branches while another operation is running"))
	}
	defer c.turn.EndExclusive()
	branches, err := c.Branches()
	if err != nil {
		return agent.BranchInfo{}, c.rewindFail(err)
	}
	match, err := resolveBranch(branches, ref)
	if err != nil {
		return agent.BranchInfo{}, c.rewindFail(err)
	}
	loaded, err := agent.LoadSession(match.Path)
	if err != nil {
		return agent.BranchInfo{}, c.rewindFail(err)
	}
	if c.executor != nil {
		c.executor.SetSession(loaded)
	}
	c.session.SetPath(match.Path)
	c.ckpt.Rebind(match.Path)
	c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo,
		Text: fmt.Sprintf("switched to branch %s", branchDisplayName(match))})
	return match, nil
}

func resolveBranch(branches []agent.BranchInfo, ref string) (agent.BranchInfo, error) {
	refLower := strings.ToLower(ref)
	var matches []agent.BranchInfo
	for _, b := range branches {
		nameLower := strings.ToLower(strings.TrimSpace(b.Name))
		switch {
		case b.ID == ref || strings.EqualFold(b.ID, ref):
			return b, nil
		case b.Name != "" && nameLower == refLower:
			matches = append(matches, b)
		case strings.HasPrefix(strings.ToLower(b.ID), refLower):
			matches = append(matches, b)
		case strings.HasPrefix(strings.ToLower(shortBranchID(b.ID)), refLower):
			matches = append(matches, b)
		case b.Path == ref:
			return b, nil
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return agent.BranchInfo{}, fmt.Errorf("branch %q is ambiguous", ref)
	}
	return agent.BranchInfo{}, fmt.Errorf("branch %q not found", ref)
}

func branchDisplayName(b agent.BranchInfo) string {
	if strings.TrimSpace(b.Name) != "" {
		return fmt.Sprintf("%s (%s)", b.Name, b.ID)
	}
	return b.ID
}
