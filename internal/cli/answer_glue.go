// answer_glue.go holds the write-side store port the serve command needs but
// no wave-1 package owns: the durable project board's full-fidelity
// loadBoard/saveBoard, applyHumanAnswer, recomputeReadiness, and the
// repo-level answer application with HTTP semantics (all
// src/orchestrator/store.ts + src/orchestrator/types.ts).
//
// state_glue.go's loadProjectBoard decodes only the fields CLI surfaces read;
// the answer route must WRITE the board back, so this file decodes every
// field into a raw, key-order-preserving object tree: saveBoard re-emits
// unknown fields untouched (JSON.parse + object spread semantics), keeping
// the 2-space JSON.stringify format byte-compatible.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// boardFile mirrors store.ts's board path: <repoPath>/.devagent-project.json.
const boardFile = ".devagent-project.json"

// isoFormat mirrors Node's Date.prototype.toISOString()
// (always UTC, millisecond precision, trailing Z).
const isoFormat = "2006-01-02T15:04:05.000Z07:00"

// marshalCompact renders v as compact JSON without HTML escaping —
// JSON.stringify semantics (Node never escapes < > &).
func marshalCompact(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("null") // unreachable for the types used here
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")) // Encode appends a newline
}

// rawObject is a decoded JSON object that remembers key order and stores the
// untouched raw value of every field. First-seen key position is kept even
// when a key repeats (last value wins) — exactly what JSON.parse produces
// before JSON.stringify re-emits it.
type rawObject struct {
	order []string
	vals  map[string]json.RawMessage
}

func newRawObject() *rawObject {
	return &rawObject{vals: make(map[string]json.RawMessage)}
}

// decodeRawObject parses exactly one JSON object from raw.
func decodeRawObject(raw []byte) (*rawObject, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("not a JSON object")
	}
	o := newRawObject()
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, fmt.Errorf("non-string object key")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o.set(key, v)
	}
	if _, err := dec.Token(); err != nil { // consume the closing '}'
		return nil, err
	}
	return o, nil
}

// set stores a value, keeping an existing key's position or appending a new
// key at the end (object spread semantics: {...obj, k: v} keeps k's original
// slot when it already existed).
func (o *rawObject) set(key string, v json.RawMessage) {
	if _, dup := o.vals[key]; !dup {
		o.order = append(o.order, key)
	}
	o.vals[key] = v
}

// remove deletes a key outright — JSON.stringify drops undefined-valued
// fields (store.ts: `t.evidenceGaps = undefined`).
func (o *rawObject) remove(key string) {
	if _, dup := o.vals[key]; !dup {
		return
	}
	delete(o.vals, key)
	for i, k := range o.order {
		if k == key {
			o.order = append(o.order[:i], o.order[i+1:]...)
			break
		}
	}
}

// MarshalJSON emits a compact object in the preserved key order. Callers run
// it through a json.Encoder with SetIndent("", "  ") + SetEscapeHTML(false),
// which re-indents this output into the JSON.stringify(_, null, 2) shape.
func (o *rawObject) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.order {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(marshalCompact(k))
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// glueTaskView is the typed slice of OrchestratorTask the answer path and
// recomputeReadiness read (src/orchestrator/types.ts). Everything else on the
// task stays raw in glueTask.raw.
type glueTaskView struct {
	ID            string   `json:"id"`
	Status        string   `json:"status"`
	Prompt        string   `json:"prompt"`
	FailureDetail *string  `json:"failureDetail"`
	DependsOn     []string `json:"dependsOn"`
	EvidenceGaps  []string `json:"evidenceGaps"`
}

// glueTask pairs the typed view with the raw object for byte-faithful
// write-back.
type glueTask struct {
	raw  *rawObject
	view glueTaskView
}

// glueBoard is the loaded ProjectBoard: raw top-level object (goal, roles,
// failureClass, ...) plus the decoded task list.
type glueBoard struct {
	raw   *rawObject
	goal  string
	tasks []*glueTask
}

// loadBoard mirrors store.ts loadBoard(): read <repoPath>/.devagent-project.json
// and return nil when the file is missing, corrupt, or fails the TS shape
// check (`!Array.isArray(raw.tasks) || typeof raw.goal !== 'string'`).
func loadBoard(repoPath string) *glueBoard {
	raw, err := os.ReadFile(filepath.Join(repoPath, boardFile))
	if err != nil {
		return nil
	}
	obj, err := decodeRawObject(raw)
	if err != nil {
		return nil // corrupted board: caller decides to re-plan
	}
	var goal string
	if g, ok := obj.vals["goal"]; !ok || json.Unmarshal(g, &goal) != nil {
		return nil
	}
	var taskObjs []json.RawMessage
	// json.Unmarshal treats `null` as a no-op, but TS Array.isArray(null) is
	// false — require a real array.
	if t, ok := obj.vals["tasks"]; !ok || json.Unmarshal(t, &taskObjs) != nil || taskObjs == nil {
		return nil
	}
	board := &glueBoard{raw: obj, goal: goal}
	for _, tr := range taskObjs {
		tobj, err := decodeRawObject(tr)
		if err != nil {
			return nil // a non-object task element: treat the board as corrupt
		}
		var view glueTaskView
		if err := json.Unmarshal(tr, &view); err != nil {
			return nil
		}
		board.tasks = append(board.tasks, &glueTask{raw: tobj, view: view})
	}
	return board
}

// saveBoard mirrors store.ts saveBoard(): stamp updatedAt, recompute
// readiness, write 2-space JSON with a trailing newline atomically
// (tmp+rename so a crash mid-write never corrupts state).
func saveBoard(repoPath string, board *glueBoard) error {
	recomputeReadiness(board.tasks)
	next := newRawObject()
	for _, k := range board.raw.order {
		next.set(k, board.raw.vals[k])
	}
	taskVals := make([]json.RawMessage, len(board.tasks))
	for i, t := range board.tasks {
		b, err := t.raw.MarshalJSON()
		if err != nil {
			return err
		}
		taskVals[i] = b
	}
	next.set("updatedAt", marshalCompact(time.Now().UTC().Format(isoFormat)))
	next.set("tasks", marshalCompact(taskVals))

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ") // Encoder's trailing newline is the TS `+ "\n"`
	if err := enc.Encode(next); err != nil {
		return err
	}
	file := filepath.Join(repoPath, boardFile)
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, out.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// applyHumanAnswer mirrors store.ts applyHumanAnswer(): fold a human answer
// into a paused ('ask') task's contract, clear the prior evidence gaps, and
// reset the status so the next attempt is re-verified against it.
func applyHumanAnswer(board *glueBoard, taskID, answer string) (bool, string) {
	id := strings.TrimSpace(taskID)
	text := strings.TrimSpace(answer)
	var t *glueTask
	for _, cand := range board.tasks {
		if cand.view.ID == id {
			t = cand
			break
		}
	}
	if t == nil || t.view.Status != "ask" || text == "" {
		return false, fmt.Sprintf("no task '%s' paused for input (or empty answer)", id)
	}
	question := "prior question"
	if t.view.FailureDetail != nil {
		question = *t.view.FailureDetail
	}
	t.view.Prompt += fmt.Sprintf("\n\nHuman answer to \"%s\": %s", question, text)
	t.raw.set("prompt", marshalCompact(t.view.Prompt))
	t.view.EvidenceGaps = nil
	t.raw.remove("evidenceGaps") // t.evidenceGaps = undefined drops the key
	t.view.Status = "pending"
	t.raw.set("status", marshalCompact("pending"))
	return true, fmt.Sprintf("Answered %s; task back in queue.", id)
}

// recomputeReadiness mirrors types.ts recomputeReadiness(): `pending` and
// dependency-induced `blocked` are both derived states, re-derived on every
// save so a task blocked by an upstream `ask`/`blocked` is promoted once that
// upstream reaches `done` (loop-55 lesson). Statuses are read from the
// pre-pass snapshot exactly like the TS byId map built before tasks.map.
func recomputeReadiness(tasks []*glueTask) {
	statusByID := make(map[string]string, len(tasks))
	for _, t := range tasks {
		statusByID[t.view.ID] = t.view.Status
	}
	for _, t := range tasks {
		// Skip re-derivation for terminal states (done/failed/ask/...).
		if t.view.Status != "pending" && t.view.Status != "blocked" {
			continue
		}
		var (
			dangling     bool
			allDone      = true
			upstreamDead bool
		)
		for _, dep := range t.view.DependsOn {
			s, ok := statusByID[dep]
			if !ok {
				dangling = true
				break
			}
			if s != "done" {
				allDone = false
			}
			if s == "failed" || s == "blocked" || s == "ask" {
				upstreamDead = true
			}
		}
		// Dangling dependency reference: block rather than run blind. All
		// deps done (empty deps included, like TS every([])) -> ready.
		// Upstream failed permanently or paused on human input -> blocked.
		next := ""
		switch {
		case dangling:
			next = "blocked"
		case allDone:
			next = "ready"
		case upstreamDead:
			next = "blocked"
		}
		if next != "" && next != t.view.Status {
			t.view.Status = next
			t.raw.set("status", marshalCompact(next))
		}
	}
}

// answerBody is the {ok, note} shape both the endpoint result and the HTTP
// response carry (store.ts AnswerEndpointResult.body).
type answerBody struct {
	OK   bool   `json:"ok"`
	Note string `json:"note"`
}

// applyAnswerToRepo mirrors store.ts applyAnswerToRepo(): repo-level answer
// application with HTTP semantics, shared by the serve command's
// POST /api/answer route (LangGraph interrupt/resume pattern: observation and
// resumption are separate endpoints; decisions bind explicitly to a task id
// and reject cleanly when state moved on).
func applyAnswerToRepo(repoPath, taskID, answer string) (int, answerBody) {
	if strings.TrimSpace(answer) == "" {
		return 400, answerBody{OK: false, Note: "empty answer"}
	}
	board := loadBoard(repoPath)
	if board == nil {
		return 404, answerBody{OK: false, Note: "no project board for this repo"}
	}
	ok, note := applyHumanAnswer(board, taskID, answer)
	if !ok {
		// unknown id or task no longer in 'ask': a stale/duplicate decision
		// is a conflict
		return 409, answerBody{OK: false, Note: note}
	}
	// TS ignores saveBoard write errors (`saveBoard(...)` void) and returns
	// the success body regardless — mirror that.
	_ = saveBoard(repoPath, board)
	return 200, answerBody{OK: true, Note: note}
}
