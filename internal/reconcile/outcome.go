package reconcile

import "strings"

type Difference struct {
	System string `json:"system"`
	Host   string `json:"host,omitempty"`
	Item   string `json:"item"`
}

type Differences []Difference

func (d Differences) Error() string {
	items := make([]string, 0, len(d))
	for _, difference := range d {
		item := difference.System + ": "
		if difference.Host != "" {
			item += difference.Host + ": "
		}
		items = append(items, item+difference.Item)
	}
	return "production differs from its declaration: " + strings.Join(items, "; ")
}

const (
	OutcomeMatches = "matches"
	OutcomeDiffers = "differs"
	OutcomeFailed  = "failed"
)

type Verification struct {
	Revision    string       `json:"revision,omitempty"`
	Deep        bool         `json:"deep"`
	Outcome     string       `json:"outcome"`
	Differences []Difference `json:"differences"`
	Errors      []string     `json:"errors"`
}

func VerificationOutcome(revision string, deep bool, err error) Verification {
	verification := Verification{Revision: revision, Deep: deep, Differences: []Difference{}, Errors: []string{}}
	var visit func(error)
	visit = func(err error) {
		switch err := err.(type) {
		case nil:
		case Differences:
			verification.Differences = append(verification.Differences, err...)
		case interface{ Unwrap() []error }:
			for _, inner := range err.Unwrap() {
				visit(inner)
			}
		default:
			verification.Errors = append(verification.Errors, err.Error())
		}
	}
	visit(err)
	switch {
	case len(verification.Differences) > 0:
		verification.Outcome = OutcomeDiffers
	case len(verification.Errors) > 0:
		verification.Outcome = OutcomeFailed
	default:
		verification.Outcome = OutcomeMatches
	}
	return verification
}
