package corporateaction

import (
	"sort"
	"sync"
	"time"
)

// Store 为公司行动模块的持久化边界:保存行动定义、登记日快照、
// 转换明细、执行结果、状态变化历史与账户流水。
// 当前实现为线程安全的内存存储,可整体替换为数据库实现。
type Store struct {
	mu sync.Mutex

	actions     map[string]*Action
	postings    []Posting
	postingKeys map[string]struct{}
	snapshots   map[string][]SnapshotRow
	conversions map[string]map[string]*Conversion
	executions  map[string]*ExecutionResult
	history     map[string][]StatusChange
}

func NewStore() *Store {
	return &Store{
		actions:     make(map[string]*Action),
		postingKeys: make(map[string]struct{}),
		snapshots:   make(map[string][]SnapshotRow),
		conversions: make(map[string]map[string]*Conversion),
		executions:  make(map[string]*ExecutionResult),
		history:     make(map[string][]StatusChange),
	}
}

func (st *Store) getAction(actionID string) (*Action, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	a, ok := st.actions[actionID]
	if !ok {
		return nil, false
	}
	cp := *a
	return &cp, true
}

// putAction 整体替换行动记录(调用方需持有服务层锁并传入副本)。
func (st *Store) putAction(a *Action) {
	st.mu.Lock()
	defer st.mu.Unlock()
	cp := *a
	st.actions[a.ActionID] = &cp
}

func (st *Store) actionExists(actionID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.actions[actionID]
	return ok
}

// listActions 返回全部行动副本,按行动号排序。
func (st *Store) listActions() []Action {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]Action, 0, len(st.actions))
	for _, a := range st.actions {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ActionID < out[j].ActionID })
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

// holdings 汇总某持有人截至 asOf(含)的总份额与冻结份额。
func (st *Store) holdings(fundID, shareClass, holderID string, asOf Date) (total, frozen Shares) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, p := range st.postings {
		if p.FundID == fundID && p.ShareClass == shareClass &&
			p.HolderID == holderID && !p.Date.After(asOf) {
			total += p.ShareDelta
			frozen += p.FrozenDelta
		}
	}
	return total, frozen
}

// currentHoldings 汇总某持有人当前(不考虑日期)的总份额与冻结份额。
func (st *Store) currentHoldings(fundID, shareClass, holderID string) (total, frozen Shares) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, p := range st.postings {
		if p.FundID == fundID && p.ShareClass == shareClass && p.HolderID == holderID {
			total += p.ShareDelta
			frozen += p.FrozenDelta
		}
	}
	return total, frozen
}

// cashBalance 汇总某持有人的现金余额。
func (st *Store) cashBalance(fundID, shareClass, holderID string) Money {
	st.mu.Lock()
	defer st.mu.Unlock()
	var total Money
	for _, p := range st.postings {
		if p.FundID == fundID && p.ShareClass == shareClass && p.HolderID == holderID {
			total += p.CashDelta
		}
	}
	return total
}

func (st *Store) saveSnapshot(actionID string, rows []SnapshotRow) {
	st.mu.Lock()
	defer st.mu.Unlock()
	cp := make([]SnapshotRow, len(rows))
	copy(cp, rows)
	st.snapshots[actionID] = cp
}

func (st *Store) snapshot(actionID string) []SnapshotRow {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]SnapshotRow, len(st.snapshots[actionID]))
	copy(out, st.snapshots[actionID])
	return out
}

// saveConversions 仅在行动首次进入执行时调用,每名持有人一条。
func (st *Store) saveConversions(convs []*Conversion) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, c := range convs {
		m := st.conversions[c.ActionID]
		if m == nil {
			m = make(map[string]*Conversion)
			st.conversions[c.ActionID] = m
		}
		cp := *c
		m[c.HolderID] = &cp
	}
}

func (st *Store) conversionsOf(actionID string) []Conversion {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]Conversion, 0, len(st.conversions[actionID]))
	for _, c := range st.conversions[actionID] {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HolderID < out[j].HolderID })
	return out
}

// pendingHolders 返回未完成明细的持有人列表(确定性顺序)。
func (st *Store) pendingHolders(actionID string) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for h, c := range st.conversions[actionID] {
		if c.Status == ConversionPending {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// applyConversion 将单条转换明细分录入账并置为完成。
// 旧份额保留为负数注销流水,新份额与零碎现金为正数流水;
// 入账键幂等,重复调用不会再次增加或减少份额。
func (st *Store) applyConversion(c *Conversion, effectiveDate Date, at time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	existing := st.conversions[c.ActionID][c.HolderID]
	if existing == nil || existing.Status == ConversionDone {
		return
	}
	retireKey := "ca:" + c.ActionID + ":" + c.HolderID + ":retire"
	issueKey := "ca:" + c.ActionID + ":" + c.HolderID + ":issue"
	if _, done := st.postingKeys[retireKey]; !done {
		st.postingKeys[retireKey] = struct{}{}
		st.postings = append(st.postings, Posting{
			Key: retireKey, FundID: c.FundID, ShareClass: c.ShareClass,
			HolderID: c.HolderID, Date: effectiveDate,
			ShareDelta:  -c.BeforeShares,
			FrozenDelta: -c.BeforeFrozen,
			Kind:        PostingConversionRetire, Ref: c.ActionID,
		})
	}
	if _, done := st.postingKeys[issueKey]; !done {
		st.postingKeys[issueKey] = struct{}{}
		st.postings = append(st.postings, Posting{
			Key: issueKey, FundID: c.FundID, ShareClass: c.ShareClass,
			HolderID: c.HolderID, Date: effectiveDate,
			ShareDelta:  c.AfterShares,
			FrozenDelta: c.AfterFrozen,
			CashDelta:   c.CashInLieu,
			Kind:        PostingConversionIssue, Ref: c.ActionID,
		})
	}
	existing.Status = ConversionDone
	existing.FinishedAt = at
}

func (st *Store) saveExecution(res *ExecutionResult) {
	st.mu.Lock()
	defer st.mu.Unlock()
	cp := *res
	st.executions[res.ActionID] = &cp
}

func (st *Store) executionOf(actionID string) (*ExecutionResult, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	r, ok := st.executions[actionID]
	if !ok {
		return nil, false
	}
	cp := *r
	return &cp, true
}

func (st *Store) addHistory(sc StatusChange) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.history[sc.ActionID] = append(st.history[sc.ActionID], sc)
}

func (st *Store) historyOf(actionID string) []StatusChange {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]StatusChange, len(st.history[actionID]))
	copy(out, st.history[actionID])
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
