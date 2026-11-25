package settlement

// PriceUnit is the unit a model is priced in.
//
// A model is priced in exactly ONE unit, declared in the catalog (ModelInfo.PriceUnit).
// The unit comes from CONFIGURATION, never from a worker's receipt: a worker attests
// what it PRODUCED (a quantity, an asset hash), and the gateway prices that in the unit
// the catalog declares for the model it routed to. A receipt claiming a different unit
// than the catalog is a mismatch to reject, not an instruction to follow.
//
// That direction is deliberate. The token pipeline already works this way — the unit is
// a per-model constant, only the QUANTITY is reported by the worker, and it is
// cross-checked against what the gateway parsed itself. Letting a signed receipt pick
// the unit would let a worker route around the catalog validation that refuses a
// mispriced model.
//
// Typed (not a bare string) so that a unit can never be silently swapped for another
// string argument in a call to the billing formula.
type PriceUnit string

// EffectiveUnit maps the zero value onto PriceUnitToken: an entry that declares no
// price_unit is a token model, so every catalog entry predating unit generalisation
// keeps its meaning without edits.
func EffectiveUnit(u PriceUnit) PriceUnit {
	if u == "" {
		return PriceUnitToken
	}
	return u
}

// IsToken reports whether the unit is billed through the token pipeline. The empty
// unit counts as token, so callers that never mention units keep working.
func (u PriceUnit) IsToken() bool { return EffectiveUnit(u) == PriceUnitToken }
