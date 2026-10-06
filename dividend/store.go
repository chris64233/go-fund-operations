package dividend

import (
	"sort"
	"sync"
	"time"
)

// Store 是分红模块的持久化边界:保存方案、登记快照、领取选择、
// 分配明细、执行结果、状态变化历史、更正记录与账户流水。
// 当前实现为线程安全的内存存储,可整体替换为数据库实现。
type Store struct {
	mu sync.Mutex

	plans         map[string]*Plan
	postings      []Posting
	postingKeys   map[string]struct{}
	snapshots     map[string][]SnapshotRow
	choices       map[string]map[string]*Choice
	allocations   map[string]map[string]*Allocation
	executions    map[string]*ExecutionResult
	history       map[string][]StatusChange
	corrections   map[string][]Correction
	correctionIDs map[string]struct{}
}

// NewStore 创建空的内存存储。
func NewStore() *Store {
	return &Store{
		plans:         make(map[string]*Plan),
		postingKeys:   make(map[string]struct{}),
		snapshots:     make(map[string][]SnapshotRow),
		choices:       make(map[string]map[string]*Choice),
		allocations:   make(map[string]map[string]*Allocation),
		executions:    make(map[string]*ExecutionResult),
		history:       make(map[string][]StatusChange),
		corrections:   make(map[string][]Correction),
		correctionIDs: make(map[string]struct{}),
	}
}

func (st *Store) getPlan(planID string) (Plan, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	p, ok := st.plans[planID]
	if !ok {
		return Plan{}, false
	}
	return *p, true
}

// putPlan 整体替换方案记录(调用方需持有服务层锁)。
func (st *Store) putPlan(p Plan) {
	st.mu.Lock()
	defer st.mu.Unlock()
	cp := p
	st.plans[p.PlanID] = &cp
}

func (st *Store) planExists(planID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.plans[planID]
	return ok
}

// listPlans 返回全部方案副本,按方案号排序(确定性遍历)。
func (st *Store) listPlans() []Plan {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]Plan, 0, len(st.plans))
	for _, p := range st.plans {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PlanID < out[j].PlanID })
	return out
}

// addPosting 按幂等键入账;键已存在时忽略,返回是否实际入账。
func (st *Store) addPosting(p Posting) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, dup := st.postingKeys[p.Key]; dup {
		return false
	}
	st.postingKeys[p.Key] = struct{}{}
	st.postings = append(st.postings, p)
	return true
}

func (st *Store) hasPosting(key string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.postingKeys[key]
	return ok
}

// holdings 汇总某持有人截至 asOf(含)的份额。
func (st *Store) holdings(fundID, holderID string, asOf Date) Shares {
	st.mu.Lock()
	defer st.mu.Unlock()
	var total Shares
	for _, p := range st.postings {
		if p.FundID == fundID && p.HolderID == holderID && !p.Date.After(asOf) {
			total += p.ShareDelta
		}
	}
	return total
}

// totalHoldings 汇总某持有人全部份额(不考虑日期)。
func (st *Store) totalHoldings(fundID, holderID string) Shares {
	st.mu.Lock()
	defer st.mu.Unlock()
	var total Shares
	for _, p := range st.postings {
		if p.FundID == fundID && p.HolderID == holderID {
			total += p.ShareDelta
		}
	}
	return total
}

// cashBalance 汇总某持有人的现金余额。
func (st *Store) cashBalance(fundID, holderID string) Money {
	st.mu.Lock()
	defer st.mu.Unlock()
	var total Money
	for _, p := range st.postings {
		if p.FundID == fundID && p.HolderID == holderID {
			total += p.CashDelta
		}
	}
	return total
}

// saveSnapshot 冻结保存登记快照(仅在确认时调用一次)。
func (st *Store) saveSnapshot(planID string, rows []SnapshotRow) {
	st.mu.Lock()
	defer st.mu.Unlock()
	cp := make([]SnapshotRow, len(rows))
	copy(cp, rows)
	st.snapshots[planID] = cp
}

func (st *Store) snapshot(planID string) []SnapshotRow {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]SnapshotRow, len(st.snapshots[planID]))
	copy(out, st.snapshots[planID])
	sort.Slice(out, func(i, j int) bool { return out[i].HolderID < out[j].HolderID })
	return out
}

func (st *Store) setChoice(c Choice) {
	st.mu.Lock()
	defer st.mu.Unlock()
	m := st.choices[c.PlanID]
	if m == nil {
		m = make(map[string]*Choice)
		st.choices[c.PlanID] = m
	}
	cp := c
	m[c.HolderID] = &cp
}

func (st *Store) choiceOf(planID, holderID string) (Choice, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	c, ok := st.choices[planID][holderID]
	if !ok {
		return Choice{}, false
	}
	return *c, true
}

func (st *Store) choicesOf(planID string) []Choice {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]Choice, 0, len(st.choices[planID]))
	for _, c := range st.choices[planID] {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HolderID < out[j].HolderID })
	return out
}

// saveAllocations 仅在方案首次进入执行时调用,每名持有人一条。
func (st *Store) saveAllocations(allocs []Allocation) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, a := range allocs {
		m := st.allocations[a.PlanID]
		if m == nil {
			m = make(map[string]*Allocation)
			st.allocations[a.PlanID] = m
		}
		cp := a
		m[a.HolderID] = &cp
	}
}

func (st *Store) allocationsOf(planID string) []Allocation {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]Allocation, 0, len(st.allocations[planID]))
	for _, a := range st.allocations[planID] {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HolderID < out[j].HolderID })
	return out
}

// pendingAllocations 返回未完成明细的持有人列表(确定性顺序)。
func (st *Store) pendingAllocations(planID string) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for holderID, a := range st.allocations[planID] {
		if a.Status == AllocationPending {
			out = append(out, holderID)
		}
	}
	sort.Strings(out)
	return out
}

// applyAllocation 将单条明细分录入账并置为完成;
// 入账键幂等,重复调用不会重复发放现金或重复增加份额。
func (st *Store) applyAllocation(planID, holderID string, exDate Date, at time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	a := st.allocations[planID][holderID]
	if a == nil || a.Status == AllocationDone {
		return
	}
	key := "div:" + planID + ":" + holderID
	if _, done := st.postingKeys[key]; !done {
		st.postingKeys[key] = struct{}{}
		if a.CashAmount != 0 {
			st.postings = append(st.postings, Posting{
				Key: key + ":cash", FundID: a.FundID, HolderID: holderID,
				Date: exDate, CashDelta: a.CashAmount,
				Kind: PostingDividendCash, Ref: planID,
			})
			st.postingKeys[key+":cash"] = struct{}{}
		}
		if a.ReinvestShares != 0 {
			st.postings = append(st.postings, Posting{
				Key: key + ":shares", FundID: a.FundID, HolderID: holderID,
				Date: exDate, ShareDelta: a.ReinvestShares,
				Kind: PostingReinvest, Ref: planID,
			})
			st.postingKeys[key+":shares"] = struct{}{}
		}
	}
	a.Status = AllocationDone
	a.FinishedAt = at
}

func (st *Store) saveExecution(res ExecutionResult) {
	st.mu.Lock()
	defer st.mu.Unlock()
	cp := res
	st.executions[res.PlanID] = &cp
}

func (st *Store) executionOf(planID string) (ExecutionResult, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	r, ok := st.executions[planID]
	if !ok {
		return ExecutionResult{}, false
	}
	return *r, true
}

func (st *Store) addHistory(sc StatusChange) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.history[sc.PlanID] = append(st.history[sc.PlanID], sc)
}

func (st *Store) historyOf(planID string) []StatusChange {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]StatusChange, len(st.history[planID]))
	copy(out, st.history[planID])
	return out
}

func (st *Store) hasCorrection(id string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.correctionIDs[id]
	return ok
}

func (st *Store) addCorrection(c Correction, p Posting) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.correctionIDs[c.CorrectionID] = struct{}{}
	st.corrections[c.PlanID] = append(st.corrections[c.PlanID], c)
	st.postingKeys[p.Key] = struct{}{}
	st.postings = append(st.postings, p)
}

func (st *Store) correctionsOf(planID string) []Correction {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]Correction, len(st.corrections[planID]))
	copy(out, st.corrections[planID])
	return out
}

// allPostings 返回全部流水副本(供快照聚合等内部使用)。
func (st *Store) allPostings() []Posting {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]Posting, len(st.postings))
	copy(out, st.postings)
	return out
}
