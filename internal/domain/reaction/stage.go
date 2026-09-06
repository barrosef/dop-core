package reaction

import (
	"encoding/json"
	"fmt"

	"github.com/Digital-Business-One/dop-core/internal/domain/ports"
	"github.com/Digital-Business-One/dop-core/internal/platform/errs"
)

// StageActionSpec is a flow stage's action, in the narrow shape this domain
// needs. The caller reads the frozen flow version and hands these over; this
// package does not import `workflow`, because it needs one lookup from it and
// not its entities. `On` is a plain string here (not workflow.StageMoment)
// for the same reason: the two domains stay decoupled rather than sharing a
// type across a lookup boundary.
type StageActionSpec struct {
	On     string // enter | exit
	Name   ActionName
	Params map[string]string
}

// DecideStage answers what a stage transition should cause.
//
// The demand froze (flow_id, version) when it started (ADR-0014 §4), so the
// actions read here are the ones that were in force when the work began — not
// what somebody edited into the flow this morning.
//
// It does not validate action names or `On` values: the flow was validated
// when it was written (workflow.Validate). Re-checking here would make this
// function a second authority on a rule that already has one.
func DecideStage(e ports.Event, flowID string, version int32, stages map[string][]StageActionSpec) ([]PlannedAction, error) {
	var payload struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if len(e.Payload) > 0 {
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			return nil, errs.Invalid("event %s carries an unreadable payload: %v", e.ID, err)
		}
	}

	var planned []PlannedAction
	add := func(stageKey, moment string) {
		if stageKey == "" {
			return // a demand starting has no stage to leave
		}
		for _, a := range stages[stageKey] {
			if a.On != moment {
				continue
			}
			planned = append(planned, PlannedAction{
				RuleRef: fmt.Sprintf("%s/%d/%s/%s", flowID, version, stageKey, moment),
				Name:    a.Name,
				Params:  a.Params,
				Event:   e,
			})
		}
	}
	add(payload.From, "exit")
	add(payload.To, "enter")
	return planned, nil
}
