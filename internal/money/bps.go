package money

// BPS is a number of basis points. One basis point is one hundredth of one
// percent, so OneHundredPercent (10_000) is the multiplicative identity.
// Negative values are permitted (rebates, negative rates).
type BPS int64

// OneHundredPercent is 10_000 basis points.
const OneHundredPercent BPS = 10_000

// ApplyUSD returns u × b / 10_000 rounded in the given mode.
// It is USD.MulBPS with the receiver and argument swapped.
func (b BPS) ApplyUSD(u USD, mode RoundingMode) (USD, error) {
	return u.MulBPS(b, mode)
}

// ApplyQuantity returns q × b / 10_000 rounded in the given mode.
// It is Quantity.MulBPSChecked with the receiver and argument swapped.
func (b BPS) ApplyQuantity(q Quantity, mode RoundingMode) (Quantity, error) {
	return q.MulBPSChecked(b, mode)
}

// String renders the value as a percentage with two decimals, for example
// BPS(150).String() == "1.50%" and BPS(-25).String() == "-0.25%".
func (b BPS) String() string { return formatCents(int64(b)) + "%" }
