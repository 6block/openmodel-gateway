package settlement

import "testing"

// EffectiveUnit maps the zero value onto token, so a catalog entry written before
// billing units existed is a token model without any edit. Getting this wrong would
// silently re-price every legacy model.
func TestEffectiveUnit_EmptyMeansToken(t *testing.T) {
	cases := []struct {
		in   PriceUnit
		want PriceUnit
	}{
		{"", PriceUnitToken},
		{PriceUnitToken, PriceUnitToken},
		{PriceUnitImage, PriceUnitImage},
		{PriceUnitAudioSecond, PriceUnitAudioSecond},
	}
	for _, c := range cases {
		if got := EffectiveUnit(c.in); got != c.want {
			t.Errorf("EffectiveUnit(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The empty unit must count as token so that every pre-existing caller that never
// mentions a unit keeps taking the token path.
func TestPriceUnit_IsToken(t *testing.T) {
	for _, u := range []PriceUnit{"", PriceUnitToken} {
		if !u.IsToken() {
			t.Errorf("IsToken() must be true for %q", u)
		}
	}
	for _, u := range []PriceUnit{PriceUnitImage, PriceUnitAudioSecond} {
		if u.IsToken() {
			t.Errorf("IsToken() must be false for %q", u)
		}
	}
}

// ModelInfo.EffectivePriceUnit applies the same default at the catalog-entry level.
func TestModelInfo_EffectivePriceUnit(t *testing.T) {
	if got := (ModelInfo{}).EffectivePriceUnit(); got != PriceUnitToken {
		t.Errorf("an entry with no price_unit must be a token model, got %q", got)
	}
	if got := (ModelInfo{PriceUnit: PriceUnitImage}).EffectivePriceUnit(); got != PriceUnitImage {
		t.Errorf("explicit unit must survive, got %q", got)
	}
}
