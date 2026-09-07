package modelid

import "testing"

func TestValidateModelId(t *testing.T) {
	tests := []struct {
		name   string
		worker string
		model  string
		want   string // "" = acceptable
	}{
		{"omp unset is default", "omp", "", ""},
		{"omp qualified passes", "omp", "omniroute/bai/glm-5.3-flash", ""},
		{"omp tier alias rejected", "omp", "coding", `worker "omp" requires a provider-qualified model id ("provider/model", e.g. omniroute/bai/glm-5.3-flash); got "coding" (driver tier aliases like "coding" are not valid omp ids)`},
		{"pi qualified passes", "pi", "omniroute/bai/glm-5.3-flash", ""},
		{"pi tier alias rejected", "pi", "coding", `worker "pi" requires a provider-qualified model id ("provider/model", e.g. omniroute/bai/glm-5.3-flash); got "coding" (driver tier aliases like "coding" are not valid pi ids)`},
		{"grok exact slug passes", "grok", "grok-4.6", ""},
		{"grok dated pin passes", "grok", "grok-4.6-2026-08-14", ""},
		{"grok xai prefix passes", "grok", "xai/grok-4.6", ""},
		{"grok alias rejected", "grok", "coding", `worker "grok" requires an exact xAI model slug ("grok-4.6", "grok-build-0.1", dated pins) or an "xai/"-qualified id; got "coding" (driver tier aliases like "coding" are not valid grok ids)`},
		{"claude-code passthrough", "claude-code", "anything-goes", ""},
		{"opencode passthrough", "opencode", "also-anything", ""},
		{"unknown worker passthrough", "mystery", "coding", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidateModelId(tt.worker, tt.model)
			if got != tt.want {
				t.Errorf("ValidateModelId(%q, %q) = %q, want %q", tt.worker, tt.model, got, tt.want)
			}
		})
	}
}

func TestHasDeclaredModelIdShape(t *testing.T) {
	for _, worker := range []string{"omp", "pi", "grok", "claude-code", "opencode"} {
		if !HasDeclaredModelIdShape(worker) {
			t.Errorf("HasDeclaredModelIdShape(%q) = false, want true", worker)
		}
	}
	if HasDeclaredModelIdShape("mystery") {
		t.Error("HasDeclaredModelIdShape(mystery) = true, want false")
	}
}

func TestIsGrokModelId(t *testing.T) {
	for _, want := range []string{"grok-4.6", "grok-build-0.1", "grok-4.3", "xai/grok-4.6", "grok-4.6-2026-08-14"} {
		if !IsGrokModelId(want) {
			t.Errorf("IsGrokModelId(%q) = false, want true", want)
		}
	}
	for _, dont := range []string{"", "coding", "free", "grok", "x-grok-4.6", "grok/4.6"} {
		if IsGrokModelId(dont) {
			t.Errorf("IsGrokModelId(%q) = true, want false", dont)
		}
	}
}
