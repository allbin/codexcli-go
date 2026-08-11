package schema

import "encoding/json"

// Model is one entry of the `model/list` reply.
//
// This is the app-server's live model registry shape (camelCase). It is a
// different, smaller projection than the snake_case entries in codex's
// on-disk models_cache.json — see codexcli.ModelInfo for that one. Do not
// unmarshal one into the other.
type Model struct {
	// ID is the model identifier to pass as `model` on thread/turn start.
	ID string `json:"id"`
	// Model is the underlying model slug. Usually equal to ID.
	Model string `json:"model,omitempty"`

	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`

	// Hidden marks models kept out of the default picker. They are only
	// returned when ModelListParams.IncludeHidden is set.
	Hidden bool `json:"hidden,omitempty"`
	// IsDefault marks the model codex selects when none is requested.
	IsDefault bool `json:"isDefault,omitempty"`

	SupportedReasoningEfforts []ModelReasoningEffort `json:"supportedReasoningEfforts,omitempty"`
	DefaultReasoningEffort    *string                `json:"defaultReasoningEffort,omitempty"`

	// InputModalities lists what the model accepts, e.g. ["text","image"].
	InputModalities []string `json:"inputModalities,omitempty"`
	// SupportsPersonality reports whether TurnStartParams.Personality is
	// honored for this model.
	SupportsPersonality bool `json:"supportsPersonality,omitempty"`

	AdditionalSpeedTiers []string           `json:"additionalSpeedTiers,omitempty"`
	ServiceTiers         []ModelServiceTier `json:"serviceTiers,omitempty"`
	DefaultServiceTier   *string            `json:"defaultServiceTier,omitempty"`

	ModelSpecialty *string `json:"modelSpecialty,omitempty"`

	// Upgrade and UpgradeInfo point at a successor model when codex flags
	// this one as superseded. Shapes are still in flux upstream, so they
	// stay raw.
	Upgrade     json.RawMessage `json:"upgrade,omitempty"`
	UpgradeInfo json.RawMessage `json:"upgradeInfo,omitempty"`
	// AvailabilityNux is the first-run message for newly available models.
	AvailabilityNux json.RawMessage `json:"availabilityNux,omitempty"`

	// Raw preserves the full entry for fields not yet typed here.
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps both the typed projection and the raw bytes.
func (m *Model) UnmarshalJSON(data []byte) error {
	type alias Model
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*m = Model(a)
	m.Raw = append(m.Raw[:0], data...)
	return nil
}

// ModelReasoningEffort is one supported reasoning effort for a model.
type ModelReasoningEffort struct {
	ReasoningEffort string `json:"reasoningEffort"`
	Description     string `json:"description,omitempty"`
}

// ModelServiceTier describes one billing/throughput tier offered for a model.
type ModelServiceTier struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}
