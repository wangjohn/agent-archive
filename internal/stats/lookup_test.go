package stats

// Lookup returns the price of a model id as the archive records it (any
// case, with or without a date or context suffix), and whether the table
// prices it. Only the tests look prices up by id; Compute prices through the
// table's index, so this asks that same index and cannot drift from what
// Compute does.
func (t PriceTable) Lookup(model string) (ModelPrice, bool) {
	entry, ok := t.index()[NormalizeModel(model)]
	return entry, ok
}
