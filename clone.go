package fundoperations

// 返回值一律克隆，避免调用方持有内部可变指针。

func cloneNAV(v *NAVVersion) *NAVVersion {
	if v == nil {
		return nil
	}
	cp := *v
	return &cp
}

func cloneConfirmation(c *Confirmation) *Confirmation {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

func cloneAdjustment(a *Adjustment) *Adjustment {
	if a == nil {
		return nil
	}
	cp := *a
	return &cp
}
