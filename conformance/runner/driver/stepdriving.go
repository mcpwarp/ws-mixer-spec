package driver

import (
	"encoding/json"
	"os"
	"strconv"
)

// StepDriving is the parsed conformance/scenarios/step-driving.json sidecar
// (docs/CONFORMANCE.md section 3.3).
type StepDriving struct {
	overrides map[string]string // fmt.Sprintf("%s#%d", fixture, stepIndex) -> "auto"|"command"
}

func LoadStepDriving(path string) (*StepDriving, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &StepDriving{overrides: map[string]string{}}, nil
		}
		return nil, err
	}
	var doc struct {
		Overrides []struct {
			Fixture   string `json:"fixture"`
			StepIndex int    `json:"step_index"`
			Driver    string `json:"driver"`
		} `json:"overrides"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	sd := &StepDriving{overrides: map[string]string{}}
	for _, o := range doc.Overrides {
		sd.overrides[key(o.Fixture, o.StepIndex)] = o.Driver
	}
	return sd, nil
}

func key(fixture string, step int) string {
	return fixture + "#" + strconv.Itoa(step)
}

// Override returns the forced driver ("auto" or "command") for this
// {fixture, step}, or "" if there is none.
func (sd *StepDriving) Override(fixture string, step int) string {
	if sd == nil {
		return ""
	}
	return sd.overrides[key(fixture, step)]
}
