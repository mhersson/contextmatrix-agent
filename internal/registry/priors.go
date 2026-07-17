package registry

// PriorEntry holds external quality scores for a model (e.g. from Artificial
// Analysis). Role scores are pointers so an omitted role is distinguishable
// from an explicit zero: absent/null decodes to nil, meaning no prior exists.
type PriorEntry struct {
	Coder    *float64 `json:"coder"`
	Reviewer *float64 `json:"reviewer"`
}

// Priors is the parsed model-priors document.
type Priors struct {
	Models map[string]PriorEntry `json:"models"`
}

// ForRole returns the prior score for the given model and role, and whether one
// exists. A role omitted from the model's JSON entry has no prior - (0, false);
// an explicit 0 in the file is a real prior and returns (0, true). Only
// RoleCoder and RoleReviewer are tracked in priors; other roles return (0, false).
func (p Priors) ForRole(model string, role Role) (float64, bool) {
	entry, ok := p.Models[model]
	if !ok {
		return 0, false
	}

	var score *float64

	switch role {
	case RoleCoder:
		score = entry.Coder
	case RoleReviewer:
		score = entry.Reviewer
	}

	if score == nil {
		return 0, false
	}

	return *score, true
}
