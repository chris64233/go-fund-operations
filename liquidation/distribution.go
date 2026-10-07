package liquidation

import "math/big"

// allocate distributes total by frozen shares with a deterministic rule:
// each investor gets floor(total * shares / totalShares); the leftover
// is the rounding dust kept in the liquidation assets.
// big.Int avoids int64 overflow and keeps results deterministic.
func allocate(total Money, snapshot []SnapshotEntry, totalShares Shares) (amounts []Money, dust Money) {
	amounts = make([]Money, len(snapshot))
	if totalShares <= 0 {
		return amounts, total
	}
	t := big.NewInt(int64(total))
	ts := big.NewInt(int64(totalShares))
	var sum Money
	for i, e := range snapshot {
		if e.Shares <= 0 {
			continue
		}
		amt := new(big.Int).Mul(t, big.NewInt(int64(e.Shares)))
		amt.Quo(amt, ts)
		amounts[i] = Money(amt.Int64())
		sum += amounts[i]
	}
	return amounts, total - sum
}
