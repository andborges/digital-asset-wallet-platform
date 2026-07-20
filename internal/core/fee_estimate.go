package core

import "math/big"

// FeeEstimate is a withdrawal's estimated on-chain cost, split into its two components
// (Story 3.1) — the L2 execution fee and the amortized L1 data-posting fee — never
// collapsed into one undifferentiated number: an L2's naive single-number gas estimate
// systematically undercharges by omitting the L1 component entirely. All three fields are
// base-unit integers (wei), never floats. TotalFee always equals L2Fee + L1Fee.
type FeeEstimate struct {
	L2Fee    *big.Int
	L1Fee    *big.Int
	TotalFee *big.Int
}
