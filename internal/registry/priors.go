package registry

// PriorEntry holds a model's normalised quality scores per role, the shape
// the test-facing constructor takes. Role scores are pointers for source
// compatibility with existing fixtures; a nil score and a zero score mean
// the same thing to the selector, no measured prior for that role, because
// the shared package reads 0 as unmeasured (CM encodes a missing Artificial
// Analysis index as 0 on the wire and a normalised index is never genuinely
// 0).
type PriorEntry struct {
	Coder    *float64 `json:"coder"`
	Reviewer *float64 `json:"reviewer"`
}

// Priors is the per-model priors table NewRegistryFromParts converts into
// wire candidates. A model absent from Models has no prior for either role.
type Priors struct {
	Models map[string]PriorEntry `json:"models"`
}
